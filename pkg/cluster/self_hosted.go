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
	"fmt"
	"math"
	"time"

	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil"
	"github.com/coreos/etcd-operator/pkg/util/retryutil"

	"github.com/pborman/uuid"
)

func (c *Cluster) addOneSelfHostedMember() error {
	c.status.AppendScalingUpCondition(c.members.Size(), c.cluster.Spec.Size)

	newMember := c.newMember(c.memberCounter)
	c.memberCounter++
	peerURL := newMember.PeerURL()
	initialCluster := append(c.members.PeerURLPairs(), newMember.Name+"="+peerURL)

	ns := c.cluster.Metadata.Namespace
	pod := k8sutil.NewSelfHostedEtcdPod(newMember, initialCluster, c.members.ClientURLs(), c.cluster.Metadata.Name, "existing", "", c.cluster.Spec, c.cluster.AsOwner())

	_, err := c.config.KubeCli.CoreV1().Pods(ns).Create(pod)
	if err != nil {
		return err
	}
	// wait for the new pod to start and add itself into the etcd cluster.
	oldN := c.members.Size()
	err = retryutil.Retry(10*time.Second, 6, func() (bool, error) {
		err = c.updateMembers(c.members)
		if err != nil {
			c.logger.Warningf("unable to update members: %v", err)
			return false, nil
		}
		if c.members.Size() > oldN {
			return true, nil
		}
		c.logger.Infof("still waiting for the new self hosted member (%s) to start...", newMember.Name)
		return false, nil
	})
	if err != nil {
		c.logger.Warningf("failed to add member (%s) due to scheduling failure. Removing its pod (%s)", newMember.Name, pod.Name)
		// our reconcile loop assumes that no new member is added without control.
		retryutil.Retry(10*time.Second, math.MaxInt64, func() (bool, error) {
			err := c.removePod(newMember.Name)
			if err != nil {
				c.logger.Errorf("failed to delete pod (%s), retry later: %v", newMember.Name, err)
				return false, nil
			}
			return true, nil
		})
		c.logger.Infof("pod (%s) has been removed", pod.Name)
		return nil
	}

	c.logger.Infof("added a self-hosted member (%s)", newMember.Name)
	return nil
}

func (c *Cluster) newSelfHostedSeedMember() error {
	newMember := c.newMember(c.memberCounter)
	c.memberCounter++
	initialCluster := []string{newMember.Name + "=" + newMember.PeerURL()}

	pod := k8sutil.NewSelfHostedEtcdPod(newMember, initialCluster, nil, c.cluster.Metadata.Name, "new", uuid.New(), c.cluster.Spec, c.cluster.AsOwner())
	_, err := k8sutil.CreateAndWaitPod(c.config.KubeCli, c.cluster.Metadata.Namespace, pod, 30*time.Second)
	if err != nil {
		return err
	}

	c.logger.Infof("self-hosted cluster created with seed member (%s)", newMember.Name)
	return nil
}

func (c *Cluster) migrateBootMember() error {
	endpoint := c.cluster.Spec.SelfHosted.BootMemberClientEndpoint

	c.logger.Infof("migrating boot member (%s)", endpoint)

	resp, err := etcdutil.ListMembers([]string{endpoint}, c.tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to list members from boot member (%v)", err)
	}
	if len(resp.Members) != 1 {
		return fmt.Errorf("boot cluster contains more than one member")
	}
	bootMember := resp.Members[0]

	initialCluster := make([]string, 0)
	for _, purl := range bootMember.PeerURLs {
		initialCluster = append(initialCluster, fmt.Sprintf("%s=%s", bootMember.Name, purl))
	}

	// create the member inside Kubernetes for migration
	newMember := c.newMember(c.memberCounter)
	c.memberCounter++

	peerURL := newMember.PeerURL()
	initialCluster = append(initialCluster, newMember.Name+"="+peerURL)

	pod := k8sutil.NewSelfHostedEtcdPod(newMember, initialCluster, []string{endpoint}, c.cluster.Metadata.Name, "existing", "", c.cluster.Spec, c.cluster.AsOwner())
	ns := c.cluster.Metadata.Namespace
	_, err = k8sutil.CreateAndWaitPod(c.config.KubeCli, ns, pod, 30*time.Second)
	if err != nil {
		return err
	}

	go func() {
		// TODO: a shorter timeout?
		// Waiting here for cluster to get stable:
		// - etcd data are replicated;
		// - cluster TPR state has switched to "Running"
		delay := 60 * time.Second
		c.logger.Infof("wait %v before removing the boot member", delay)
		time.Sleep(delay)

		err = etcdutil.RemoveMember([]string{newMember.ClientAddr()}, c.tlsConfig, bootMember.ID)
		if err != nil {
			c.logger.Errorf("boot member migration: failed to remove the boot member (%v)", err)
		}
	}()

	c.logger.Infof("self-hosted cluster created with boot member (%s)", endpoint)

	return nil
}
