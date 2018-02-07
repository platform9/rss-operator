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

package etcdutil

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/beekhof/galera-operator/pkg/util"
)

var (
	logger = util.GetLogger("member")
)

type Member struct {
	Name string
	// Kubernetes namespace this member runs in.
	Namespace string

	SEQ        uint64
	Failures   uint64
	Online     bool
	AppPrimary bool
	AppRunning bool
	AppFailed  bool

	SecurePeer   bool
	SecureClient bool
}

func (m *Member) Addr() string {
	return fmt.Sprintf("%s.%s.%s.svc", m.Name, clusterNameFromMemberName(m.Name), m.Namespace)
}

// ClientURL is the client URL for this member
func (m *Member) ClientURL() string {
	return fmt.Sprintf("%s://%s:2379", m.clientScheme(), m.Addr())
}

func (m *Member) clientScheme() string {
	if m.SecureClient {
		return "https"
	}
	return "http"
}

func (m *Member) peerScheme() string {
	if m.SecurePeer {
		return "https"
	}
	return "http"
}

func (m *Member) String() string {
	if !m.Online {
		return fmt.Sprintf("%v-", m.Name)
	} else if m.AppFailed {
		return fmt.Sprintf("%v!", m.Name)
	} else if m.AppRunning {
		return fmt.Sprintf("%v+", m.Name)
	}
	return m.Name
}

func (m *Member) ListenClientURL() string {
	return fmt.Sprintf("%s://0.0.0.0:2379", m.clientScheme())
}
func (m *Member) ListenPeerURL() string {
	return fmt.Sprintf("%s://0.0.0.0:2380", m.peerScheme())
}

func (m *Member) PeerURL() string {
	return fmt.Sprintf("%s://%s:2380", m.peerScheme(), m.Addr())
}

type MemberSet map[string]*Member

func NewMemberSet(ms ...*Member) MemberSet {
	res := MemberSet{}
	for _, m := range ms {
		res[m.Name] = m
	}
	return res
}

// the set of all members of s1 that are not members of s2
func (ms MemberSet) Diff(other MemberSet) MemberSet {
	diff := MemberSet{}
	for n, m := range ms {
		if _, ok := other[n]; !ok {
			diff[n] = m
		} else if other[n].Online != m.Online {
			diff[n] = m
		}
	}
	return diff
}

func (m *Member) Restore(last *Member) {
	// Restore app state
	m.AppPrimary = last.AppPrimary
	m.AppRunning = last.AppRunning
	m.AppFailed = last.AppFailed
	m.Failures = last.Failures
	m.SEQ = last.SEQ
}

func (ms MemberSet) Reconcile(running MemberSet, max int) (MemberSet, error) {
	// The only thing we take from 'running' is new members and the value of .Online
	for _, last := range ms {
		if m, ok := running[last.Name]; ok {
			m.Restore(last)
		}
	}

	lostMembers := ms.Diff(running)
	newMembers := running.Diff(ms)

	if lostMembers.Size() > 0 || newMembers.Size() > 0 {
		logger.Infof("Updating membership: new=%s, lost=%s", newMembers, lostMembers)
	}

	for _, m := range newMembers {
		logger.Infof("Pod %s available", m.Name)
	}

	if len(running) <= max {
		// If we're scaling down, there is no need to restore lost members
		for _, m := range lostMembers {
			if _, ok := running[m.Name]; !ok {
				running[m.Name] = m
			}
			running[m.Name].Offline()
		}
	}
	// if running.AppPrimaries() < ms.AppPrimaries() && running.AppPrimaries() < max/2+1 {
	// 	logger.Warnf("Quorum lost")
	// 	// return running, fmt.Errorf("Quorum lost")
	// }
	return running, nil
}

// IsEqual tells whether two member sets are equal by checking
// - they have the same set of members and member equality are judged by Name only.
func (ms MemberSet) IsEqual(other MemberSet) bool {
	if ms.Size() != other.Size() {
		return false
	}
	for n := range ms {
		if _, ok := other[n]; !ok {
			return false
		}
	}
	return true
}

func (ms MemberSet) Size() int {
	return len(ms)
}

func (ms MemberSet) AppPrimaries() int {
	count := 0
	for _, m := range ms {
		if m.Online && m.AppPrimary && m.AppRunning {
			count += 1
		}
	}
	return count
}

func (ms MemberSet) AppMembers() int {
	count := 0
	for _, m := range ms {
		if m.Online && m.AppRunning {
			count += 1
		}
	}
	return count
}

func (ms MemberSet) ActiveMembers() int {
	count := 0
	for _, m := range ms {
		if m.Online {
			count += 1
		}
	}
	return count
}

func (ms MemberSet) InActiveMembers() int {
	count := 0
	for _, m := range ms {
		if !m.Online {
			count += 1
		}
	}
	return count
}

func (ms MemberSet) String() string {
	var mstring []string

	for _, m := range ms {
		mstring = append(mstring, m.String())
	}
	sort.Strings(mstring)
	return fmt.Sprintf("[%v]", strings.Join(mstring, ", "))
}

func (ms MemberSet) PickOne() *Member {
	for _, m := range ms {
		return m
	}
	panic("empty")
}

func (ms MemberSet) PeerURLPairs() []string {
	ps := make([]string, 0)
	for _, m := range ms {
		ps = append(ps, fmt.Sprintf("%s=%s", m.Name, m.PeerURL()))
	}
	return ps
}

func (ms MemberSet) Add(m *Member) {
	ms[m.Name] = m
}

func (ms MemberSet) Remove(name string) {
	delete(ms, name)
}

func (m *Member) Offline() {
	if m.Online {
		logger.Warnf("Pod %v offline", m.Name)
	}
	m.Online = false
	m.AppPrimary = false
	m.AppRunning = false
	m.AppFailed = false
}

func (ms MemberSet) ClientURLs() []string {
	endpoints := make([]string, 0, len(ms))
	for _, m := range ms {
		endpoints = append(endpoints, m.ClientURL())
	}
	return endpoints
}

func GetCounterFromMemberName(name string) (int, error) {
	i := strings.LastIndex(name, "-")
	if i == -1 || i+1 >= len(name) {
		return 0, fmt.Errorf("name (%s) does not contain '-' or anything after '-'", name)
	}
	c, err := strconv.Atoi(name[i+1:])
	if err != nil {
		return 0, fmt.Errorf("could not atoi %s: %v", name[i+1:], err)
	}
	return c, nil
}

var validPeerURL = regexp.MustCompile(`^\w+:\/\/[\w\.\-]+(:\d+)?$`)

func MemberNameFromPeerURL(pu string) (string, error) {
	// url.Parse has very loose validation. We do our own validation.
	if !validPeerURL.MatchString(pu) {
		return "", errors.New("invalid PeerURL format")
	}
	u, err := url.Parse(pu)
	if err != nil {
		return "", err
	}
	path := strings.Split(u.Host, ":")[0]
	name := strings.Split(path, ".")[0]
	return name, err
}

func CreateMemberName(clusterName string, member int) string {
	return fmt.Sprintf("%s-%04d", clusterName, member)
}

func clusterNameFromMemberName(mn string) string {
	i := strings.LastIndex(mn, "-")
	if i == -1 {
		panic(fmt.Sprintf("unexpected member name: %s", mn))
	}
	return mn[:i]
}
