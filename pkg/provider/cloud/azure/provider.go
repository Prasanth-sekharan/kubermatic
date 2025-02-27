/*
Copyright 2020 The Kubermatic Kubernetes Platform contributors.

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

package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2018-06-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-06-01/network"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-02-01/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	"go.uber.org/zap"

	kubermaticv1 "k8c.io/kubermatic/v2/pkg/crd/kubermatic/v1"
	kuberneteshelper "k8c.io/kubermatic/v2/pkg/kubernetes"
	"k8c.io/kubermatic/v2/pkg/log"
	"k8c.io/kubermatic/v2/pkg/provider"
	kubermaticresources "k8c.io/kubermatic/v2/pkg/resources"
)

const (
	resourceNamePrefix = "kubernetes-"

	clusterTagKey = "cluster"

	// FinalizerSecurityGroup will instruct the deletion of the security group
	FinalizerSecurityGroup = "kubermatic.io/cleanup-azure-security-group"
	// FinalizerRouteTable will instruct the deletion of the route table
	FinalizerRouteTable = "kubermatic.io/cleanup-azure-route-table"
	// FinalizerSubnet will instruct the deletion of the subnet
	FinalizerSubnet = "kubermatic.io/cleanup-azure-subnet"
	// FinalizerVNet will instruct the deletion of the virtual network
	FinalizerVNet = "kubermatic.io/cleanup-azure-vnet"
	// FinalizerResourceGroup will instruct the deletion of the resource group
	FinalizerResourceGroup = "kubermatic.io/cleanup-azure-resource-group"
	// FinalizerAvailabilitySet will instruct the deletion of the availability set
	FinalizerAvailabilitySet = "kubermatic.io/cleanup-azure-availability-set"

	denyAllTCPSecGroupRuleName   = "deny_all_tcp"
	denyAllUDPSecGroupRuleName   = "deny_all_udp"
	allowAllICMPSecGroupRuleName = "icmp_by_allow_all"
)

type Azure struct {
	dc                *kubermaticv1.DatacenterSpecAzure
	log               *zap.SugaredLogger
	ctx               context.Context
	secretKeySelector provider.SecretKeySelectorValueFunc
}

// New returns a new Azure provider.
func New(dc *kubermaticv1.Datacenter, secretKeyGetter provider.SecretKeySelectorValueFunc) (*Azure, error) {
	if dc.Spec.Azure == nil {
		return nil, errors.New("datacenter is not an Azure datacenter")
	}
	return &Azure{
		dc:                dc.Spec.Azure,
		log:               log.Logger,
		ctx:               context.TODO(),
		secretKeySelector: secretKeyGetter,
	}, nil
}

// Azure API doesn't allow programmatically getting the number of available fault domains in a given region.
// We must therefore hardcode these based on https://docs.microsoft.com/en-us/azure/virtual-machines/windows/manage-availability
//
// The list of region codes was generated by `az account list-locations | jq .[].id --raw-output | cut -d/ -f5 | sed -e 's/^/"/' -e 's/$/" : ,/'`
var faultDomainsPerRegion = map[string]int32{
	"eastasia":           2,
	"southeastasia":      2,
	"centralus":          3,
	"eastus":             3,
	"eastus2":            3,
	"westus":             3,
	"northcentralus":     3,
	"southcentralus":     3,
	"northeurope":        3,
	"westeurope":         3,
	"japanwest":          2,
	"japaneast":          2,
	"brazilsouth":        2,
	"australiaeast":      2,
	"australiasoutheast": 2,
	"southindia":         2,
	"centralindia":       2,
	"westindia":          2,
	"canadacentral":      3,
	"canadaeast":         2,
	"uksouth":            2,
	"ukwest":             2,
	"westcentralus":      2,
	"westus2":            2,
	"koreacentral":       2,
	"koreasouth":         2,
}

func deleteSubnet(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	subnetsClient, err := getSubnetsClient(cloud, credentials)
	if err != nil {
		return err
	}

	deleteSubnetFuture, err := subnetsClient.Delete(ctx, cloud.Azure.ResourceGroup, cloud.Azure.VNetName, cloud.Azure.SubnetName)
	if err != nil {
		return err
	}

	if err = deleteSubnetFuture.WaitForCompletionRef(ctx, subnetsClient.Client); err != nil {
		return err
	}

	return nil
}

func deleteAvailabilitySet(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	asClient, err := getAvailabilitySetClient(cloud, credentials)
	if err != nil {
		return err
	}

	_, err = asClient.Delete(ctx, cloud.Azure.ResourceGroup, cloud.Azure.AvailabilitySet)
	return err
}

func deleteVNet(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	networksClient, err := getNetworksClient(cloud, credentials)
	if err != nil {
		return err
	}

	deleteVNetFuture, err := networksClient.Delete(ctx, cloud.Azure.ResourceGroup, cloud.Azure.VNetName)
	if err != nil {
		return err
	}

	if err = deleteVNetFuture.WaitForCompletionRef(ctx, networksClient.Client); err != nil {
		return err
	}

	return nil
}

func deleteResourceGroup(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	groupsClient, err := getGroupsClient(cloud, credentials)
	if err != nil {
		return err
	}

	// We're doing a Get to see if its already gone or not.
	// We could also directly call delete but the error response would need to be unpacked twice to get the correct error message.
	// Doing a get is simpler.
	if _, err := groupsClient.Get(ctx, cloud.Azure.ResourceGroup); err != nil {
		return err
	}

	future, err := groupsClient.Delete(ctx, cloud.Azure.ResourceGroup)
	if err != nil {
		return err
	}

	if err = future.WaitForCompletionRef(ctx, groupsClient.Client); err != nil {
		return err
	}

	return nil
}

func deleteRouteTable(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	routeTablesClient, err := getRouteTablesClient(cloud, credentials)
	if err != nil {
		return err
	}

	future, err := routeTablesClient.Delete(ctx, cloud.Azure.ResourceGroup, cloud.Azure.RouteTableName)
	if err != nil {
		return err
	}

	if err = future.WaitForCompletionRef(ctx, routeTablesClient.Client); err != nil {
		return err
	}

	return nil
}

func deleteSecurityGroup(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	securityGroupsClient, err := getSecurityGroupsClient(cloud, credentials)
	if err != nil {
		return err
	}

	future, err := securityGroupsClient.Delete(ctx, cloud.Azure.ResourceGroup, cloud.Azure.SecurityGroup)
	if err != nil {
		return err
	}

	if err = future.WaitForCompletionRef(ctx, securityGroupsClient.Client); err != nil {
		return err
	}

	return nil
}

func (a *Azure) CleanUpCloudProvider(cluster *kubermaticv1.Cluster, update provider.ClusterUpdater) (*kubermaticv1.Cluster, error) {
	var err error

	credentials, err := GetCredentialsForCluster(cluster.Spec.Cloud, a.secretKeySelector)
	if err != nil {
		return nil, err
	}

	logger := a.log.With("cluster", cluster.Name)
	if kuberneteshelper.HasFinalizer(cluster, FinalizerSecurityGroup) {
		logger.Infow("deleting security group", "group", cluster.Spec.Cloud.Azure.SecurityGroup)
		if err := deleteSecurityGroup(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			if detErr, ok := err.(autorest.DetailedError); !ok || detErr.StatusCode != http.StatusNotFound {
				return cluster, fmt.Errorf("failed to delete security group %q: %v", cluster.Spec.Cloud.Azure.SecurityGroup, err)
			}
		}
		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			kuberneteshelper.RemoveFinalizer(updatedCluster, FinalizerSecurityGroup)
		})
		if err != nil {
			return nil, err
		}
	}

	if kuberneteshelper.HasFinalizer(cluster, FinalizerRouteTable) {
		logger.Infow("deleting route table", "routeTableName", cluster.Spec.Cloud.Azure.RouteTableName)
		if err := deleteRouteTable(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			if detErr, ok := err.(autorest.DetailedError); !ok || detErr.StatusCode != http.StatusNotFound {
				return cluster, fmt.Errorf("failed to delete route table %q: %v", cluster.Spec.Cloud.Azure.RouteTableName, err)
			}
		}
		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			kuberneteshelper.RemoveFinalizer(updatedCluster, FinalizerRouteTable)
		})
		if err != nil {
			return nil, err
		}
	}

	if kuberneteshelper.HasFinalizer(cluster, FinalizerSubnet) {
		logger.Infow("deleting subnet", "subnet", cluster.Spec.Cloud.Azure.SubnetName)
		if err := deleteSubnet(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			if detErr, ok := err.(autorest.DetailedError); !ok || detErr.StatusCode != http.StatusNotFound {
				return cluster, fmt.Errorf("failed to delete sub-network %q: %v", cluster.Spec.Cloud.Azure.SubnetName, err)
			}
		}
		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			kuberneteshelper.RemoveFinalizer(updatedCluster, FinalizerSubnet)
		})
		if err != nil {
			return nil, err
		}
	}

	if kuberneteshelper.HasFinalizer(cluster, FinalizerVNet) {
		logger.Infow("deleting vnet", "vnet", cluster.Spec.Cloud.Azure.VNetName)
		if err := deleteVNet(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			if detErr, ok := err.(autorest.DetailedError); !ok || detErr.StatusCode != http.StatusNotFound {
				return cluster, fmt.Errorf("failed to delete virtual network %q: %v", cluster.Spec.Cloud.Azure.VNetName, err)
			}
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			kuberneteshelper.RemoveFinalizer(updatedCluster, FinalizerVNet)
		})
		if err != nil {
			return nil, err
		}
	}

	if kuberneteshelper.HasFinalizer(cluster, FinalizerResourceGroup) {
		logger.Infow("deleting resource group", "resourceGroup", cluster.Spec.Cloud.Azure.ResourceGroup)
		if err := deleteResourceGroup(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			if detErr, ok := err.(autorest.DetailedError); !ok || detErr.StatusCode != http.StatusNotFound {
				return cluster, fmt.Errorf("failed to delete resource group %q: %v", cluster.Spec.Cloud.Azure.ResourceGroup, err)
			}
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			kuberneteshelper.RemoveFinalizer(updatedCluster, FinalizerResourceGroup)
		})
		if err != nil {
			return nil, err
		}
	}

	if kuberneteshelper.HasFinalizer(cluster, FinalizerAvailabilitySet) {
		logger.Infow("deleting availability set", "availabilitySet", cluster.Spec.Cloud.Azure.AvailabilitySet)
		if err := deleteAvailabilitySet(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			if detErr, ok := err.(autorest.DetailedError); !ok || detErr.StatusCode != http.StatusNotFound {
				return cluster, fmt.Errorf("failed to delete availability set %q: %v", cluster.Spec.Cloud.Azure.AvailabilitySet, err)
			}
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			kuberneteshelper.RemoveFinalizer(updatedCluster, FinalizerAvailabilitySet)
		})
		if err != nil {
			return nil, err
		}
	}

	return cluster, nil
}

// ensureResourceGroup will create or update an Azure resource group. The call is idempotent.
func ensureResourceGroup(ctx context.Context, cloud kubermaticv1.CloudSpec, location string, clusterName string, credentials Credentials) error {
	groupsClient, err := getGroupsClient(cloud, credentials)
	if err != nil {
		return err
	}

	parameters := resources.Group{
		Name:     to.StringPtr(cloud.Azure.ResourceGroup),
		Location: to.StringPtr(location),
		Tags: map[string]*string{
			clusterTagKey: to.StringPtr(clusterName),
		},
	}
	if _, err = groupsClient.CreateOrUpdate(ctx, cloud.Azure.ResourceGroup, parameters); err != nil {
		return fmt.Errorf("failed to create or update resource group %q: %v", cloud.Azure.ResourceGroup, err)
	}

	return nil
}

// ensureSecurityGroup will create or update an Azure security group. The call is idempotent.
func (a *Azure) ensureSecurityGroup(cloud kubermaticv1.CloudSpec, location string, clusterName string, credentials Credentials) error {
	sgClient, err := getSecurityGroupsClient(cloud, credentials)
	if err != nil {
		return err
	}

	parameters := network.SecurityGroup{
		Name:     to.StringPtr(cloud.Azure.SecurityGroup),
		Location: to.StringPtr(location),
		Tags: map[string]*string{
			clusterTagKey: to.StringPtr(clusterName),
		},
		SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
			Subnets: &[]network.Subnet{
				{
					Name: to.StringPtr(cloud.Azure.SubnetName),
					ID:   to.StringPtr(assembleSubnetID(cloud)),
				},
			},
			// inbound
			SecurityRules: &[]network.SecurityRule{
				{
					Name: to.StringPtr("ssh_ingress"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Direction:                network.SecurityRuleDirectionInbound,
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourceAddressPrefix:      to.StringPtr("*"),
						SourcePortRange:          to.StringPtr("*"),
						DestinationAddressPrefix: to.StringPtr("*"),
						DestinationPortRange:     to.StringPtr("22"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 to.Int32Ptr(100),
					},
				},
				{
					Name: to.StringPtr("inter_node_comm"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Direction:                network.SecurityRuleDirectionInbound,
						Protocol:                 "*",
						SourceAddressPrefix:      to.StringPtr("VirtualNetwork"),
						SourcePortRange:          to.StringPtr("*"),
						DestinationAddressPrefix: to.StringPtr("VirtualNetwork"),
						DestinationPortRange:     to.StringPtr("*"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 to.Int32Ptr(200),
					},
				},
				{
					Name: to.StringPtr("azure_load_balancer"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Direction:                network.SecurityRuleDirectionInbound,
						Protocol:                 "*",
						SourceAddressPrefix:      to.StringPtr("AzureLoadBalancer"),
						SourcePortRange:          to.StringPtr("*"),
						DestinationAddressPrefix: to.StringPtr("*"),
						DestinationPortRange:     to.StringPtr("*"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 to.Int32Ptr(300),
					},
				},
				// outbound
				{
					Name: to.StringPtr("outbound_allow_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Direction:                network.SecurityRuleDirectionOutbound,
						Protocol:                 "*",
						SourceAddressPrefix:      to.StringPtr("*"),
						SourcePortRange:          to.StringPtr("*"),
						DestinationAddressPrefix: to.StringPtr("*"),
						DestinationPortRange:     to.StringPtr("*"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 to.Int32Ptr(100),
					},
				},
			},
		},
	}

	updatedRules := append(*parameters.SecurityRules, tcpDenyAllRule(), udpDenyAllRule(), icmpAllowAllRule())
	parameters.SecurityRules = &updatedRules

	if _, err = sgClient.CreateOrUpdate(a.ctx, cloud.Azure.ResourceGroup, cloud.Azure.SecurityGroup, parameters); err != nil {
		return fmt.Errorf("failed to create or update resource group %q: %v", cloud.Azure.ResourceGroup, err)
	}

	return nil
}

// ensureVNet will create or update an Azure virtual network in the specified resource group. The call is idempotent.
func ensureVNet(ctx context.Context, cloud kubermaticv1.CloudSpec, location string, clusterName string, credentials Credentials) error {
	networksClient, err := getNetworksClient(cloud, credentials)
	if err != nil {
		return err
	}

	parameters := network.VirtualNetwork{
		Name:     to.StringPtr(cloud.Azure.VNetName),
		Location: to.StringPtr(location),
		Tags: map[string]*string{
			clusterTagKey: to.StringPtr(clusterName),
		},
		VirtualNetworkPropertiesFormat: &network.VirtualNetworkPropertiesFormat{
			AddressSpace: &network.AddressSpace{AddressPrefixes: &[]string{"10.0.0.0/16"}},
		},
	}

	var resourceGroup = cloud.Azure.ResourceGroup
	if cloud.Azure.VNetResourceGroup != "" {
		resourceGroup = cloud.Azure.VNetResourceGroup
	}
	future, err := networksClient.CreateOrUpdate(ctx, resourceGroup, cloud.Azure.VNetName, parameters)
	if err != nil {
		return fmt.Errorf("failed to create or update virtual network %q: %v", cloud.Azure.VNetName, err)
	}

	if err = future.WaitForCompletionRef(ctx, networksClient.Client); err != nil {
		return fmt.Errorf("failed to create or update virtual network %q: %v", cloud.Azure.VNetName, err)
	}

	return nil
}

// ensureSubnet will create or update an Azure subnetwork in the specified vnet. The call is idempotent.
func ensureSubnet(ctx context.Context, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	subnetsClient, err := getSubnetsClient(cloud, credentials)
	if err != nil {
		return err
	}

	parameters := network.Subnet{
		Name: to.StringPtr(cloud.Azure.SubnetName),
		SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
			AddressPrefix: to.StringPtr("10.0.0.0/16"),
		},
	}

	var resourceGroup = cloud.Azure.ResourceGroup
	if cloud.Azure.VNetResourceGroup != "" {
		resourceGroup = cloud.Azure.VNetResourceGroup
	}
	future, err := subnetsClient.CreateOrUpdate(ctx, resourceGroup, cloud.Azure.VNetName, cloud.Azure.SubnetName, parameters)
	if err != nil {
		return fmt.Errorf("failed to create or update subnetwork %q: %v", cloud.Azure.SubnetName, err)
	}

	if err = future.WaitForCompletionRef(ctx, subnetsClient.Client); err != nil {
		return fmt.Errorf("failed to create or update subnetwork %q: %v", cloud.Azure.SubnetName, err)
	}

	return nil
}

// ensureRouteTable will create or update an Azure route table attached to the specified subnet. The call is idempotent.
func ensureRouteTable(ctx context.Context, cloud kubermaticv1.CloudSpec, location string, credentials Credentials) error {
	routeTablesClient, err := getRouteTablesClient(cloud, credentials)
	if err != nil {
		return err
	}

	parameters := network.RouteTable{
		Name:     to.StringPtr(cloud.Azure.RouteTableName),
		Location: to.StringPtr(location),
		RouteTablePropertiesFormat: &network.RouteTablePropertiesFormat{
			Subnets: &[]network.Subnet{
				{
					Name: to.StringPtr(cloud.Azure.SubnetName),
					ID:   to.StringPtr(assembleSubnetID(cloud)),
				},
			},
		},
	}

	future, err := routeTablesClient.CreateOrUpdate(ctx, cloud.Azure.ResourceGroup, cloud.Azure.RouteTableName, parameters)
	if err != nil {
		return fmt.Errorf("failed to create or update route table %q: %v", cloud.Azure.RouteTableName, err)
	}

	if err = future.WaitForCompletionRef(ctx, routeTablesClient.Client); err != nil {
		return fmt.Errorf("failed to create or update route table %q: %v", cloud.Azure.RouteTableName, err)
	}

	return nil
}

func (a *Azure) InitializeCloudProvider(cluster *kubermaticv1.Cluster, update provider.ClusterUpdater) (*kubermaticv1.Cluster, error) {
	var err error
	logger := a.log.With("cluster", cluster.Name)
	location := a.dc.Location

	credentials, err := GetCredentialsForCluster(cluster.Spec.Cloud, a.secretKeySelector)
	if err != nil {
		return nil, err
	}

	if cluster.Spec.Cloud.Azure.ResourceGroup == "" {
		cluster.Spec.Cloud.Azure.ResourceGroup = resourceNamePrefix + cluster.Name

		logger.Infow("ensuring resource group", "resourceGroup", cluster.Spec.Cloud.Azure.ResourceGroup)
		if err = ensureResourceGroup(a.ctx, cluster.Spec.Cloud, location, cluster.Name, credentials); err != nil {
			return cluster, err
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			updatedCluster.Spec.Cloud.Azure.ResourceGroup = cluster.Spec.Cloud.Azure.ResourceGroup
			kuberneteshelper.AddFinalizer(updatedCluster, FinalizerResourceGroup)
		})
		if err != nil {
			return nil, err
		}
	}

	if cluster.Spec.Cloud.Azure.VNetName == "" {
		cluster.Spec.Cloud.Azure.VNetName = resourceNamePrefix + cluster.Name

		logger.Infow("ensuring vnet", "vnet", cluster.Spec.Cloud.Azure.VNetName)
		if err = ensureVNet(a.ctx, cluster.Spec.Cloud, location, cluster.Name, credentials); err != nil {
			return cluster, err
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			updatedCluster.Spec.Cloud.Azure.VNetName = cluster.Spec.Cloud.Azure.VNetName
			kuberneteshelper.AddFinalizer(updatedCluster, FinalizerVNet)
		})
		if err != nil {
			return nil, err
		}
	}

	if cluster.Spec.Cloud.Azure.SubnetName == "" {
		cluster.Spec.Cloud.Azure.SubnetName = resourceNamePrefix + cluster.Name

		logger.Infow("ensuring subnet", "subnet", cluster.Spec.Cloud.Azure.SubnetName)
		if err = ensureSubnet(a.ctx, cluster.Spec.Cloud, credentials); err != nil {
			return cluster, err
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			updatedCluster.Spec.Cloud.Azure.SubnetName = cluster.Spec.Cloud.Azure.SubnetName
			kuberneteshelper.AddFinalizer(updatedCluster, FinalizerSubnet)
		})
		if err != nil {
			return nil, err
		}
	}

	if cluster.Spec.Cloud.Azure.RouteTableName == "" {
		cluster.Spec.Cloud.Azure.RouteTableName = resourceNamePrefix + cluster.Name

		logger.Infow("ensuring route table", "routeTableName", cluster.Spec.Cloud.Azure.RouteTableName)
		if err = ensureRouteTable(a.ctx, cluster.Spec.Cloud, location, credentials); err != nil {
			return cluster, err
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			updatedCluster.Spec.Cloud.Azure.RouteTableName = cluster.Spec.Cloud.Azure.RouteTableName
			kuberneteshelper.AddFinalizer(updatedCluster, FinalizerRouteTable)
		})
		if err != nil {
			return nil, err
		}
	}

	if cluster.Spec.Cloud.Azure.SecurityGroup == "" {
		cluster.Spec.Cloud.Azure.SecurityGroup = resourceNamePrefix + cluster.Name

		logger.Infow("ensuring security group", "securityGroup", cluster.Spec.Cloud.Azure.SecurityGroup)
		if err = a.ensureSecurityGroup(cluster.Spec.Cloud, location, cluster.Name, credentials); err != nil {
			return cluster, err
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			updatedCluster.Spec.Cloud.Azure.SecurityGroup = cluster.Spec.Cloud.Azure.SecurityGroup
			kuberneteshelper.AddFinalizer(updatedCluster, FinalizerSecurityGroup)
		})
		if err != nil {
			return nil, err
		}
	}

	if cluster.Spec.Cloud.Azure.AvailabilitySet == "" {
		asName := resourceNamePrefix + cluster.Name
		logger.Infow("ensuring AvailabilitySet", "availabilitySet", asName)

		if err := ensureAvailabilitySet(a.ctx, asName, location, cluster.Spec.Cloud, credentials); err != nil {
			return nil, fmt.Errorf("failed to ensure AvailabilitySet exists: %v", err)
		}

		cluster, err = update(cluster.Name, func(updatedCluster *kubermaticv1.Cluster) {
			updatedCluster.Spec.Cloud.Azure.AvailabilitySet = asName
			kuberneteshelper.AddFinalizer(updatedCluster, FinalizerAvailabilitySet)
		})
		if err != nil {
			return nil, err
		}
	}

	return cluster, nil
}

func ensureAvailabilitySet(ctx context.Context, name, location string, cloud kubermaticv1.CloudSpec, credentials Credentials) error {
	client, err := getAvailabilitySetClient(cloud, credentials)
	if err != nil {
		return err
	}

	faultDomainCount, ok := faultDomainsPerRegion[location]
	if !ok {
		return fmt.Errorf("could not determine the number of fault domains, unknown region %q", location)
	}

	as := compute.AvailabilitySet{
		Name:     to.StringPtr(name),
		Location: to.StringPtr(location),
		Sku: &compute.Sku{
			Name: to.StringPtr("Aligned"),
		},
		AvailabilitySetProperties: &compute.AvailabilitySetProperties{
			PlatformFaultDomainCount:  to.Int32Ptr(faultDomainCount),
			PlatformUpdateDomainCount: to.Int32Ptr(20),
		},
	}

	_, err = client.CreateOrUpdate(ctx, cloud.Azure.ResourceGroup, name, as)
	return err
}

func (a *Azure) DefaultCloudSpec(cloud *kubermaticv1.CloudSpec) error {
	return nil
}

func (a *Azure) ValidateCloudSpec(cloud kubermaticv1.CloudSpec) error {
	credentials, err := GetCredentialsForCluster(cloud, a.secretKeySelector)
	if err != nil {
		return err
	}

	if cloud.Azure.ResourceGroup != "" {
		rgClient, err := getGroupsClient(cloud, credentials)
		if err != nil {
			return err
		}

		if _, err = rgClient.Get(a.ctx, cloud.Azure.ResourceGroup); err != nil {
			return err
		}
	}

	var resourceGroup = cloud.Azure.ResourceGroup
	if cloud.Azure.VNetResourceGroup != "" {
		resourceGroup = cloud.Azure.VNetResourceGroup
	}

	if cloud.Azure.VNetName != "" {
		vnetClient, err := getNetworksClient(cloud, credentials)
		if err != nil {
			return err
		}

		if _, err = vnetClient.Get(a.ctx, resourceGroup, cloud.Azure.VNetName, ""); err != nil {
			return err
		}
	}

	if cloud.Azure.SubnetName != "" {
		subnetClient, err := getSubnetsClient(cloud, credentials)
		if err != nil {
			return err
		}

		if _, err = subnetClient.Get(a.ctx, resourceGroup, cloud.Azure.VNetName, cloud.Azure.SubnetName, ""); err != nil {
			return err
		}
	}

	if cloud.Azure.RouteTableName != "" {
		routeTablesClient, err := getRouteTablesClient(cloud, credentials)
		if err != nil {
			return err
		}

		if _, err = routeTablesClient.Get(a.ctx, cloud.Azure.ResourceGroup, cloud.Azure.RouteTableName, ""); err != nil {
			return err
		}
	}

	if cloud.Azure.SecurityGroup != "" {
		sgClient, err := getSecurityGroupsClient(cloud, credentials)
		if err != nil {
			return err
		}

		if _, err = sgClient.Get(a.ctx, cloud.Azure.ResourceGroup, cloud.Azure.SecurityGroup, ""); err != nil {
			return err
		}
	}

	return nil
}

func (a *Azure) AddICMPRulesIfRequired(cluster *kubermaticv1.Cluster) error {
	credentials, err := GetCredentialsForCluster(cluster.Spec.Cloud, a.secretKeySelector)
	if err != nil {
		return err
	}

	azure := cluster.Spec.Cloud.Azure
	if azure.SecurityGroup == "" {
		return nil
	}
	sgClient, err := getSecurityGroupsClient(cluster.Spec.Cloud, credentials)
	if err != nil {
		return fmt.Errorf("failed to get security group client: %v", err)
	}
	sg, err := sgClient.Get(a.ctx, azure.ResourceGroup, azure.SecurityGroup, "")
	if err != nil {
		return fmt.Errorf("failed to get security group %q: %v", azure.SecurityGroup, err)
	}

	var hasDenyAllTCPRule, hasDenyAllUDPRule, hasICMPAllowAllRule bool
	if sg.SecurityRules != nil {
		for _, rule := range *sg.SecurityRules {
			if rule.Name == nil {
				continue
			}
			// We trust that no one will alter the content of the rules
			switch *rule.Name {
			case denyAllTCPSecGroupRuleName:
				hasDenyAllTCPRule = true
			case denyAllUDPSecGroupRuleName:
				hasDenyAllUDPRule = true
			case allowAllICMPSecGroupRuleName:
				hasICMPAllowAllRule = true
			}
		}
	}

	var newSecurityRules []network.SecurityRule
	if !hasDenyAllTCPRule {
		a.log.With("cluster", cluster.Name).Info("Creating TCP deny all rule")
		newSecurityRules = append(newSecurityRules, tcpDenyAllRule())
	}
	if !hasDenyAllUDPRule {
		a.log.With("cluster", cluster.Name).Info("Creating UDP deny all rule")
		newSecurityRules = append(newSecurityRules, udpDenyAllRule())
	}
	if !hasICMPAllowAllRule {
		a.log.With("cluster", cluster.Name).Info("Creating ICMP allow all rule")
		newSecurityRules = append(newSecurityRules, icmpAllowAllRule())
	}

	if len(newSecurityRules) > 0 {
		newSecurityGroupRules := append(*sg.SecurityRules, newSecurityRules...)
		sg.SecurityRules = &newSecurityGroupRules
		_, err := sgClient.CreateOrUpdate(a.ctx, azure.ResourceGroup, azure.SecurityGroup, sg)
		if err != nil {
			return fmt.Errorf("failed to add new rules to security group %q: %v", *sg.Name, err)
		}
	}
	return nil
}

func getGroupsClient(cloud kubermaticv1.CloudSpec, credentials Credentials) (*resources.GroupsClient, error) {
	var err error
	groupsClient := resources.NewGroupsClient(credentials.SubscriptionID)
	groupsClient.Authorizer, err = auth.NewClientCredentialsConfig(credentials.ClientID, credentials.ClientSecret, credentials.TenantID).Authorizer()
	if err != nil {
		return nil, fmt.Errorf("failed to create authorizer: %s", err.Error())
	}

	return &groupsClient, nil
}

func getNetworksClient(cloud kubermaticv1.CloudSpec, credentials Credentials) (*network.VirtualNetworksClient, error) {
	var err error
	networksClient := network.NewVirtualNetworksClient(credentials.SubscriptionID)
	networksClient.Authorizer, err = auth.NewClientCredentialsConfig(credentials.ClientID, credentials.ClientSecret, credentials.TenantID).Authorizer()
	if err != nil {
		return nil, fmt.Errorf("failed to create authorizer: %s", err.Error())
	}

	return &networksClient, nil
}

func getSubnetsClient(cloud kubermaticv1.CloudSpec, credentials Credentials) (*network.SubnetsClient, error) {
	var err error
	subnetsClient := network.NewSubnetsClient(credentials.SubscriptionID)
	subnetsClient.Authorizer, err = auth.NewClientCredentialsConfig(credentials.ClientID, credentials.ClientSecret, credentials.TenantID).Authorizer()
	if err != nil {
		return nil, fmt.Errorf("failed to create authorizer: %s", err.Error())
	}

	return &subnetsClient, nil
}

func getRouteTablesClient(cloud kubermaticv1.CloudSpec, credentials Credentials) (*network.RouteTablesClient, error) {
	var err error
	routeTablesClient := network.NewRouteTablesClient(credentials.SubscriptionID)
	routeTablesClient.Authorizer, err = auth.NewClientCredentialsConfig(credentials.ClientID, credentials.ClientSecret, credentials.TenantID).Authorizer()
	if err != nil {
		return nil, fmt.Errorf("failed to create authorizer: %s", err.Error())
	}

	return &routeTablesClient, nil
}

func getSecurityGroupsClient(cloud kubermaticv1.CloudSpec, credentials Credentials) (*network.SecurityGroupsClient, error) {
	var err error
	securityGroupsClient := network.NewSecurityGroupsClient(credentials.SubscriptionID)
	securityGroupsClient.Authorizer, err = auth.NewClientCredentialsConfig(credentials.ClientID, credentials.ClientSecret, credentials.TenantID).Authorizer()
	if err != nil {
		return nil, fmt.Errorf("failed to create authorizer: %s", err.Error())
	}

	return &securityGroupsClient, nil
}

func getAvailabilitySetClient(cloud kubermaticv1.CloudSpec, credentials Credentials) (*compute.AvailabilitySetsClient, error) {
	var err error
	asClient := compute.NewAvailabilitySetsClient(credentials.SubscriptionID)
	asClient.Authorizer, err = auth.NewClientCredentialsConfig(credentials.ClientID, credentials.ClientSecret, credentials.TenantID).Authorizer()
	if err != nil {
		return nil, fmt.Errorf("failed to create authorizer: %s", err.Error())
	}

	return &asClient, nil
}

func tcpDenyAllRule() network.SecurityRule {
	return network.SecurityRule{
		Name: to.StringPtr(denyAllTCPSecGroupRuleName),
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Direction:                network.SecurityRuleDirectionInbound,
			Protocol:                 "TCP",
			SourceAddressPrefix:      to.StringPtr("*"),
			SourcePortRange:          to.StringPtr("*"),
			DestinationPortRange:     to.StringPtr("*"),
			DestinationAddressPrefix: to.StringPtr("*"),
			Access:                   network.SecurityRuleAccessDeny,
			Priority:                 to.Int32Ptr(800),
		},
	}
}

func udpDenyAllRule() network.SecurityRule {
	return network.SecurityRule{
		Name: to.StringPtr(denyAllUDPSecGroupRuleName),
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Direction:                network.SecurityRuleDirectionInbound,
			Protocol:                 "UDP",
			SourceAddressPrefix:      to.StringPtr("*"),
			SourcePortRange:          to.StringPtr("*"),
			DestinationPortRange:     to.StringPtr("*"),
			DestinationAddressPrefix: to.StringPtr("*"),
			Access:                   network.SecurityRuleAccessDeny,
			Priority:                 to.Int32Ptr(801),
		},
	}
}

// Alright, so here's the deal. We need to allow ICMP, but on Azure it is not possible
// to specify ICMP as a protocol in a rule - only TCP or UDP.
// Therefore we're hacking around it by first blocking all incoming TCP and UDP
// and if these don't match, we have an "allow all" rule. Dirty, but the only way.
// See also: https://tinyurl.com/azure-allow-icmp
func icmpAllowAllRule() network.SecurityRule {
	return network.SecurityRule{
		Name: to.StringPtr(allowAllICMPSecGroupRuleName),
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Direction:                network.SecurityRuleDirectionInbound,
			Protocol:                 "*",
			SourceAddressPrefix:      to.StringPtr("*"),
			SourcePortRange:          to.StringPtr("*"),
			DestinationAddressPrefix: to.StringPtr("*"),
			DestinationPortRange:     to.StringPtr("*"),
			Access:                   network.SecurityRuleAccessAllow,
			Priority:                 to.Int32Ptr(900),
		},
	}
}

// ValidateCloudSpecUpdate verifies whether an update of cloud spec is valid and permitted
func (a *Azure) ValidateCloudSpecUpdate(oldSpec kubermaticv1.CloudSpec, newSpec kubermaticv1.CloudSpec) error {
	return nil
}

type Credentials struct {
	TenantID       string
	SubscriptionID string
	ClientID       string
	ClientSecret   string
}

// GetCredentialsForCluster returns the credentials for the passed in cloud spec or an error
func GetCredentialsForCluster(cloud kubermaticv1.CloudSpec, secretKeySelector provider.SecretKeySelectorValueFunc) (Credentials, error) {
	tenantID := cloud.Azure.TenantID
	subscriptionID := cloud.Azure.SubscriptionID
	clientID := cloud.Azure.ClientID
	clientSecret := cloud.Azure.ClientSecret
	var err error

	if tenantID == "" {
		if cloud.Azure.CredentialsReference == nil {
			return Credentials{}, errors.New("no credentials provided")
		}
		tenantID, err = secretKeySelector(cloud.Azure.CredentialsReference, kubermaticresources.AzureTenantID)
		if err != nil {
			return Credentials{}, err
		}
	}

	if subscriptionID == "" {
		if cloud.Azure.CredentialsReference == nil {
			return Credentials{}, errors.New("no credentials provided")
		}
		subscriptionID, err = secretKeySelector(cloud.Azure.CredentialsReference, kubermaticresources.AzureSubscriptionID)
		if err != nil {
			return Credentials{}, err
		}
	}

	if clientID == "" {
		if cloud.Azure.CredentialsReference == nil {
			return Credentials{}, errors.New("no credentials provided")
		}
		clientID, err = secretKeySelector(cloud.Azure.CredentialsReference, kubermaticresources.AzureClientID)
		if err != nil {
			return Credentials{}, err
		}
	}

	if clientSecret == "" {
		if cloud.Azure.CredentialsReference == nil {
			return Credentials{}, errors.New("no credentials provided")
		}
		clientSecret, err = secretKeySelector(cloud.Azure.CredentialsReference, kubermaticresources.AzureClientSecret)
		if err != nil {
			return Credentials{}, err
		}
	}

	return Credentials{
		TenantID:       tenantID,
		SubscriptionID: subscriptionID,
		ClientID:       clientID,
		ClientSecret:   clientSecret,
	}, nil
}
