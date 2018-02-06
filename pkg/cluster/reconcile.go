// Copyright 2016 The etcd-operator Authors
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
	"errors"
	"fmt"

	api "github.com/beekhof/galera-operator/pkg/apis/galera/v1alpha1"
	"github.com/beekhof/galera-operator/pkg/util/etcdutil"

	"k8s.io/api/core/v1"
)

// ErrLostQuorum indicates that the etcd cluster lost its quorum.
var (
	ErrLostQuorum = errors.New("lost quorum")
)

// reconcile reconciles cluster current state to desired state specified by spec.
// - it tries to reconcile the cluster to desired size.
// - if the cluster needs for upgrade, it tries to upgrade old member one by one.
func (c *Cluster) reconcile(pods []*v1.Pod) []error {
	var errors []error
	c.logger.Infoln("Start reconciling")
	defer c.logger.Infoln("Finish reconciling")

	defer func() {
		c.status.Replicas = c.peers.Size()
		c.updateCRStatus("reconcile")
	}()

	sp := c.rss.Spec
	running := c.podsToMemberSet(pods, c.isSecureClient())

	// On controller restore, we could have "members == nil"
	if c.peers == nil {
		c.peers = etcdutil.MemberSet{}
	}

	var err error
	c.peers, err = c.peers.Reconcile(running, c.rss.Spec.GetNumReplicas())
	errors = appendNonNil(errors, err)

	for _, m := range c.peers {

		// TODO: Make the threshold configurable
		// ' > 1' means that we tried at least a start and a stop
		if m.AppFailed && m.Failures > 1 {
			errors = append(errors, fmt.Errorf("%v deletion after %v failures", m.Name, m.Failures))
			errors = appendNonNil(errors, c.deleteMember(m))

		} else if !m.Online {
			c.logger.Infof("reconcile: Skipping offline pod %v", m.Name)
			continue

		} else if m.AppFailed {
			c.logger.Warnf("reconcile: Cleaning up pod %v", m.Name)
			if err := c.stopAppMember(m); err != nil {
				errors = append(errors, fmt.Errorf("%v deletion after stop failure: %v", m.Name, err))
				errors = appendNonNil(errors, c.deleteMember(m))
			}

		} else {
			_, _, err, rc := c.execute(api.StatusCommandKey, m.Name, false)

			if _, ok := c.rss.Spec.Pod.Commands[api.SecondaryCommandKey]; rc == 0 && !ok {
				// Secondaries are not in use, map to primary
				rc = 8
			}

			switch rc {
			case 0:
				if !m.AppRunning {
					c.logger.Infof("reconcile: Detected active applcation on %v: %v", m.Name, err)
				} else if m.AppPrimary {
					c.logger.Warnf("reconcile: Detected demoted primary on %v: %v", m.Name, err)
				}
				m.AppRunning = true
				m.AppPrimary = false
			case 7:
				if m.AppRunning {
					c.logger.Warnf("reconcile: Detected stopped applcation on %v: %v", m.Name, err)
				}
				m.AppRunning = false
				m.AppPrimary = false
			case 8:
				if !m.AppRunning {
					c.logger.Infof("reconcile: Detected active primary applcation on %v: %v", m.Name, err)
				} else if !m.AppPrimary {
					c.logger.Warnf("reconcile: Detected promoted secondary on %v: %v", m.Name, err)
				}
				m.AppPrimary = true
				m.AppRunning = true
			default:
				c.logger.Errorf("reconcile: Check failed on %v: %v", m.Name, err)
				m.AppRunning = true
				m.AppFailed = true
			}
		}
	}

	if c.peers.ActiveMembers() > sp.GetNumReplicas() {
		c.status.SetScalingDownCondition(c.peers.ActiveMembers(), sp.GetNumReplicas())

	} else if c.peers.ActiveMembers() < sp.GetNumReplicas() {
		c.status.SetScalingUpCondition(c.peers.ActiveMembers(), sp.GetNumReplicas())

	} else if len(errors) > 0 {
		c.status.SetRecoveringCondition()

	} else {
		c.status.SetReadyCondition()
	}

	return errors
}
