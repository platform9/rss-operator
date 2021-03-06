// Copyright 2017 Andrew Beekhof
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"fmt"
	"strconv"
	"strings"

	api "github.com/beekhof/rss-operator/pkg/apis/clusterlabs/v1alpha1"
	"github.com/beekhof/rss-operator/pkg/util"
	"github.com/beekhof/rss-operator/pkg/util/constants"
	"github.com/beekhof/rss-operator/pkg/util/k8sutil"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (c *Cluster) replicate() []error {
	c.logger.Debug("Start replication")
	defer c.logger.Debug("Finish replication")
	defer c.updateCRStatus("replicate")

	errors := []error{}
	replicas := c.rss.Spec.GetNumReplicas()
	primaries := c.rss.Spec.GetNumPrimaries()

	if c.peers.AppMembers() > replicas {
		return []error{fmt.Errorf("Waiting for %v peers to be stopped", c.peers.AppMembers()-primaries)}
	}

	if c.peers.AppPrimaries() == 0 && c.peers.ActiveMembers() != replicas {
		return []error{fmt.Errorf("Waiting for %v additional peer containers to be available before seeding", replicas-c.peers.ActiveMembers())}
	}

	// Always detect members (so that the most up-to-date ones get started)
	if c.peers.AppPrimaries() != primaries || c.peers.AppMembers() != replicas {
		c.detectMembers()
	}

	if c.peers.AppPrimaries() == 0 {
		seeds, err := chooseSeeds(c)
		if err != nil {
			return []error{fmt.Errorf("Bootstrapping failed: %v", err)}
		}

		for n, seed := range seeds {
			errors = appendNonNil(errors, c.startAppMember(seed, true))
			if c.peers.AppPrimaries() != 0 {
				break
			}
			c.logger.Warnf("Seed %v failed, %v remaining", n+1, len(seeds)-n-1)
		}

		if c.peers.AppPrimaries() == 0 {
			return []error{fmt.Errorf("Bootstrapping from %v possible seeds with version %v failed: %v", len(seeds), seeds[0].SEQ, combineErrors(errors))}
		}

		// Continue as long as one seed succeeds
		errors = []error{}
	}

	// Whichever stage we're up to... do as much as we can before exiting rather than giving up at the first error

	if len(errors) == 0 {
		for c.peers.AppPrimaries() < primaries {
			c.logger.Infof("Starting %v more primaries", primaries-c.peers.AppPrimaries())

			seed, err := chooseMember(c)
			errors = appendNonNil(errors, err)
			if err != nil {
				break
			}

			errors = appendNonNil(errors, c.startAppMember(seed, true))
			if c.peers.AppPrimaries() == 0 {
				break
			}
		}
	}

	if len(errors) == 0 {
		for c.peers.AppPrimaries() > primaries {
			c.logger.Infof("Demoting %v primaries", c.peers.AppPrimaries()-primaries)

			seed, err := chooseCurrentPrimary(c)
			errors = appendNonNil(errors, err)
			if err != nil {
				break
			}

			errors = appendNonNil(errors, c.stopAppMember(seed))
		}
	}

	if len(errors) == 0 {
		for c.peers.AppMembers() < replicas {
			c.logger.Infof("Starting %v secondaries", replicas-c.peers.AppMembers())

			seed, err := chooseMember(c)
			errors = appendNonNil(errors, err)
			if err != nil {
				break
			}

			errors = appendNonNil(errors, c.startAppMember(seed, false))
		}
	}

	status := fmt.Sprintf("%v of %v primaries, and %v of %v members available",
		c.peers.AppPrimaries(), primaries, c.peers.AppMembers(), replicas)

	if len(errors) == 0 && replicas > 0 {
		c.logger.Info(status)

	} else {
		c.logger.Errorf("%v: %v", status, combineErrors(errors))
		errors = appendAllNonNil(errors, c.recover(c.peers))
	}

	return errors
}

func (c *Cluster) detectMembers() {
	for _, m := range c.peers {
		if !m.Online {
			continue
		}
		raw, _, err, _ := c.execute(api.SequenceCommandKey, m.Name, true)
		if err == nil {
			last := c.peers[m.Name].SEQ
			stdout := strings.TrimSpace(raw)
			if stdout == "" {
				c.logger.WithField("pod", m.Name).Warnf("discover:  no output for pod %v", m.Name)

			} else {
				c.peers[m.Name].SEQ, err = strconv.ParseUint(stdout, 10, 64)
				if err != nil {
					c.logger.WithField("pod", m.Name).Errorf("discover:  pod %v: could not parse '%v' into uint64: %v", m.Name, stdout, err)
				}
			}
			if last != c.peers[m.Name].SEQ {
				c.logger.WithField("pod", m.Name).Infof("discover:  pod %v sequence now: %v (was %v)", m.Name, c.peers[m.Name].SEQ, last)
			}
		}
	}
}

func chooseSeeds(c *Cluster) ([]*util.Member, error) {
	var bestPeer []*util.Member
	if c.peers == nil {
		return bestPeer, fmt.Errorf("No known peers")
	}
	for _, m := range c.peers {
		if !m.Online || m.AppFailed {
			continue
		} else if m.AppPrimary {
			continue
		} else if len(bestPeer) == 0 {
			bestPeer = append(bestPeer, m)
		} else if m.SEQ > bestPeer[0].SEQ {
			bestPeer = []*util.Member{m}
		} else {
			bestPeer = append(bestPeer, m)
		}
	}
	if len(bestPeer) == 0 {
		return bestPeer, fmt.Errorf("No seed peers available")
	}
	return bestPeer, nil
}

func chooseMember(c *Cluster) (*util.Member, error) {
	var bestPeer *util.Member
	if c.peers == nil {
		return nil, fmt.Errorf("No known peers")
	}
	for _, m := range c.peers {
		if !m.Online || m.AppFailed {
			continue
		} else if m.AppPrimary {
			continue
		} else if bestPeer == nil {
			bestPeer = m
		} else if m.SEQ > bestPeer.SEQ {
			bestPeer = m
		} else if strings.Compare(m.Name, bestPeer.Name) < 0 {
			// Prefer sts members towards the start of the range to be a primary
			bestPeer = m
		}
	}
	if bestPeer == nil {
		return nil, fmt.Errorf("No further peers available")
	}
	return bestPeer, nil
}

func chooseCurrentPrimary(c *Cluster) (*util.Member, error) {
	var bestPeer *util.Member
	if c.peers == nil {
		return nil, fmt.Errorf("No known peers")
	}
	for _, m := range c.peers {
		if !m.AppPrimary || m.AppFailed {
			continue
		} else if !m.Online {
			return m, nil
		} else if bestPeer == nil {
			bestPeer = m
		} else if strings.Compare(m.Name, bestPeer.Name) > 0 {
			// Prefer sts members towards the end of the range to stop/demote
			bestPeer = m
		}
	}
	if bestPeer == nil {
		return nil, fmt.Errorf("No primaries remaining")
	}
	return bestPeer, nil
}

func (c *Cluster) startCommand(asPrimary bool, primaries int32) string {

	if asPrimary && primaries == 0 {
		if _, ok := c.rss.Spec.Pod.Commands[api.SeedCommandKey]; ok {
			return api.SeedCommandKey
		}

	} else if !asPrimary {
		if _, ok := c.rss.Spec.Pod.Commands[api.SecondaryCommandKey]; ok {
			return api.SecondaryCommandKey
		}
	}

	return api.PrimaryCommandKey
}

func (c *Cluster) startAppMember(m *util.Member, asPrimary bool) error {
	action := c.startCommand(asPrimary, c.peers.AppPrimaries())
	if asPrimary && c.peers.AppPrimaries() == 0 {
		c.logger.Infof("Seeding from pod %v: %v", m.Name, m.SEQ)
	}

	_, _, err, _ := c.execute(action, m.Name, false)
	if err != nil {
		m.AppFailed = true
		m.Failures += 1
		return err
	}

	c.logger.Infof("Created app %v on %v", action, m.Name)
	m.AppPrimary = asPrimary
	m.AppRunning = true
	m.AppFailed = false
	c.tagAppMember(m, true)
	return nil
}

func (c *Cluster) tagAppMember(m *util.Member, online bool) error {

	pod, err := c.config.KubeCli.CoreV1().Pods(c.rss.Namespace).Get(m.Name, metav1.GetOptions{})
	if err != nil {
		c.logger.Errorf("failed to get pod %v in order to update labels: %v", m.Name, err)
		return err
	}

	oldPod := pod.DeepCopy()

	labels := pod.GetLabels()
	if online {
		labels[constants.MemberActiveKey] = "true"

	} else {
		labels[constants.MemberActiveKey] = "false"
	}
	pod.SetLabels(labels)

	patchdata, err := k8sutil.CreatePatch(oldPod, pod, v1.Pod{})
	if err != nil {
		c.logger.Errorf("error creating label patch for %v: %v", m.Name, err)
		return err
	}

	_, err = c.config.KubeCli.CoreV1().Pods(c.rss.Namespace).Patch(m.Name, types.StrategicMergePatchType, patchdata)
	if err != nil {
		c.logger.Errorf("fail to update the labels for %s: %v", m.Name, err)
		return err
	}

	return nil
}

func (c *Cluster) stopAppMember(m *util.Member) error {
	c.tagAppMember(m, false)
	_, _, err, _ := c.execute(api.StopCommandKey, m.Name, false)
	if err != nil {
		m.AppFailed = true
		m.Failures += 1
		return err
	}

	m.AppPrimary = false
	m.AppRunning = false
	m.AppFailed = false
	return nil
}
