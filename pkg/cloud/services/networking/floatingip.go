/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package networking

import (
	"time"

	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"k8s.io/apimachinery/pkg/util/wait"

	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1alpha4"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/metrics"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/record"
)

func (s *Service) GetOrCreateFloatingIP(openStackCluster *infrav1.OpenStackCluster, ip string) (*floatingips.FloatingIP, error) {
	var fp *floatingips.FloatingIP
	var err error
	var fpCreateOpts floatingips.CreateOpts

	if ip != "" {
		fp, err = s.checkIfFloatingIPExists(ip)
		if err != nil {
			return nil, err
		}
		if fp != nil {
			return fp, nil
		}
		// only admin can add ip address
		fpCreateOpts.FloatingIP = ip
	}

	fpCreateOpts.FloatingNetworkID = openStackCluster.Status.ExternalNetwork.ID

	mc := metrics.NewMetricPrometheusContext("floating_ip", "create")
	fp, err = floatingips.Create(s.client, fpCreateOpts).Extract()
	if mc.ObserveRequest(err) != nil {
		record.Warnf(openStackCluster, "FailedCreateFloatingIP", "Failed to create floating IP %s: %v", ip, err)
		return nil, err
	}

	record.Eventf(openStackCluster, "SuccessfulCreateFloatingIP", "Created floating IP %s with id %s", fp.FloatingIP, fp.ID)
	return fp, nil
}

func (s *Service) checkIfFloatingIPExists(ip string) (*floatingips.FloatingIP, error) {
	allPages, err := floatingips.List(s.client, floatingips.ListOpts{FloatingIP: ip}).AllPages()
	if err != nil {
		return nil, err
	}
	fpList, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return nil, err
	}
	if len(fpList) == 0 {
		return nil, nil
	}
	return &fpList[0], nil
}

func (s *Service) GetFloatingIPByPortID(portID string) (*floatingips.FloatingIP, error) {
	allPages, err := floatingips.List(s.client, floatingips.ListOpts{PortID: portID}).AllPages()
	if err != nil {
		return nil, err
	}
	fpList, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return nil, err
	}
	if len(fpList) == 0 {
		return nil, nil
	}
	return &fpList[0], nil
}

func (s *Service) DeleteFloatingIP(openStackCluster *infrav1.OpenStackCluster, ip string) error {
	fip, err := s.checkIfFloatingIPExists(ip)
	if err != nil {
		return err
	}
	if fip == nil {
		// nothing to do
		return nil
	}

	mc := metrics.NewMetricPrometheusContext("floating_ip", "delete")
	err = floatingips.Delete(s.client, fip.ID).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		record.Warnf(openStackCluster, "FailedDeleteFloatingIP", "Failed to delete floating IP %s: %v", ip, err)
		return err
	}

	record.Eventf(openStackCluster, "SuccessfulDeleteFloatingIP", "Deleted floating IP %s", ip)
	return nil
}

var backoff = wait.Backoff{
	Steps:    10,
	Duration: 30 * time.Second,
	Factor:   1.0,
	Jitter:   0.1,
}

func (s *Service) AssociateFloatingIP(openStackCluster *infrav1.OpenStackCluster, fp *floatingips.FloatingIP, portID string) error {
	s.logger.Info("Associating floating IP", "id", fp.ID, "ip", fp.FloatingIP)

	fpUpdateOpts := &floatingips.UpdateOpts{
		PortID: &portID,
	}

	mc := metrics.NewMetricPrometheusContext("floating_ip", "update")
	_, err := floatingips.Update(s.client, fp.ID, fpUpdateOpts).Extract()
	if mc.ObserveRequest(err) != nil {
		record.Warnf(openStackCluster, "FailedAssociateFloatingIP", "Failed to associate floating IP %s with port %s: %v", fp.FloatingIP, portID, err)
		return err
	}

	if err = s.waitForFloatingIP(fp.ID, "ACTIVE"); err != nil {
		record.Warnf(openStackCluster, "FailedAssociateFloatingIP", "Failed to associate floating IP %s with port %s: wait for floating IP ACTIVE: %v", fp.FloatingIP, portID, err)
		return err
	}

	record.Eventf(openStackCluster, "SuccessfulAssociateFloatingIP", "Associated floating IP %s with port %s", fp.FloatingIP, portID)
	return nil
}

func (s *Service) DisassociateFloatingIP(openStackCluster *infrav1.OpenStackCluster, ip string) error {
	fip, err := s.checkIfFloatingIPExists(ip)
	if err != nil {
		return err
	}
	if fip == nil || fip.FloatingIP == "" {
		s.logger.Info("Floating IP not associated", "ip", ip)
		return nil
	}

	s.logger.Info("Disassociating floating IP", "id", fip.ID, "ip", fip.FloatingIP)

	fpUpdateOpts := &floatingips.UpdateOpts{
		PortID: nil,
	}

	mc := metrics.NewMetricPrometheusContext("floating_ip", "update")
	_, err = floatingips.Update(s.client, fip.ID, fpUpdateOpts).Extract()
	if mc.ObserveRequest(err) != nil {
		record.Warnf(openStackCluster, "FailedDisassociateFloatingIP", "Failed to disassociate floating IP %s: %v", fip.FloatingIP, err)
		return err
	}

	if err = s.waitForFloatingIP(fip.ID, "DOWN"); err != nil {
		record.Warnf(openStackCluster, "FailedDisassociateFloatingIP", "Failed to disassociate floating IP: wait for floating IP DOWN: %v", fip.FloatingIP, err)
		return err
	}

	record.Eventf(openStackCluster, "SuccessfulDisassociateFloatingIP", "Disassociated floating IP %s", fip.FloatingIP)
	return nil
}

func (s *Service) waitForFloatingIP(id, target string) error {
	s.logger.Info("Waiting for floating IP", "id", id, "targetStatus", target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		fip, err := floatingips.Get(s.client, id).Extract()
		if err != nil {
			return false, err
		}
		return fip.Status == target, nil
	})
}
