/*
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

package simulation

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// InstanceTypeCatalog represents a collection of AWS instance types captured from a region.
// This format allows offline simulation without requiring AWS API access.
type InstanceTypeCatalog struct {
	// APIVersion for the catalog format
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	// Kind is always "InstanceTypeCatalog"
	Kind string `json:"kind" yaml:"kind"`
	// Metadata about the catalog
	Metadata InstanceTypeCatalogMetadata `json:"metadata" yaml:"metadata"`
	// InstanceTypes is the list of available instance types
	InstanceTypes []CatalogInstanceType `json:"instanceTypes" yaml:"instanceTypes"`
}

// InstanceTypeCatalogMetadata contains information about the captured catalog.
type InstanceTypeCatalogMetadata struct {
	// CapturedAt is when this catalog was captured
	CapturedAt time.Time `json:"capturedAt" yaml:"capturedAt"`
	// Region is the AWS region
	Region string `json:"region" yaml:"region"`
	// Zones are the availability zones in this region
	Zones []string `json:"zones" yaml:"zones"`
	// Count is the number of instance types
	Count int `json:"count" yaml:"count"`
}

// CatalogInstanceType represents an AWS instance type with pricing information.
type CatalogInstanceType struct {
	// Name is the instance type name (e.g., "m5.large")
	Name string `json:"name" yaml:"name"`
	// CPU is the number of vCPUs
	CPU string `json:"cpu" yaml:"cpu"`
	// Memory in Kubernetes resource format (e.g., "8192Mi")
	Memory string `json:"memory" yaml:"memory"`
	// Architecture is either "amd64" or "arm64"
	Architecture string `json:"architecture" yaml:"architecture"`
	// Offerings are the available zones and capacity types with pricing
	Offerings []CatalogOffering `json:"offerings" yaml:"offerings"`
	// Capacity contains additional resource information
	Capacity CatalogCapacity `json:"capacity,omitempty" yaml:"capacity,omitempty"`
}

// CatalogOffering represents availability and pricing for a specific zone/capacity-type.
type CatalogOffering struct {
	// Zone is the availability zone (e.g., "us-west-2a")
	Zone string `json:"zone" yaml:"zone"`
	// CapacityType is "on-demand" or "spot"
	CapacityType string `json:"capacityType" yaml:"capacityType"`
	// Price is the hourly cost in USD
	Price float64 `json:"price" yaml:"price"`
	// Available indicates if this offering is currently available
	Available bool `json:"available" yaml:"available"`
}

// CatalogCapacity contains resource capacity beyond CPU/Memory.
type CatalogCapacity struct {
	// Pods is the maximum number of pods
	Pods string `json:"pods,omitempty" yaml:"pods,omitempty"`
	// EphemeralStorage in Kubernetes resource format
	EphemeralStorage string `json:"ephemeralStorage,omitempty" yaml:"ephemeralStorage,omitempty"`
	// GPU resources if any
	NvidiaGPU string `json:"nvidia.com/gpu,omitempty" yaml:"nvidia.com/gpu,omitempty"`
	AMDGPU    string `json:"amd.com/gpu,omitempty" yaml:"amd.com/gpu,omitempty"`
	// AWS-specific resources
	AWSNeuron string `json:"aws.amazon.com/neuron,omitempty" yaml:"aws.amazon.com/neuron,omitempty"`
	EFA       string `json:"vpc.amazonaws.com/efa,omitempty" yaml:"vpc.amazonaws.com/efa,omitempty"`
}

// LoadInstanceTypeCatalog reads an InstanceTypeCatalog from a YAML file.
func LoadInstanceTypeCatalog(path string) (*InstanceTypeCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading instance types file: %w", err)
	}

	catalog := &InstanceTypeCatalog{}
	if err := yaml.Unmarshal(data, catalog); err != nil {
		return nil, fmt.Errorf("parsing instance types YAML: %w", err)
	}

	// Validate required fields
	if catalog.APIVersion == "" {
		return nil, fmt.Errorf("instance types catalog missing apiVersion")
	}
	if catalog.Kind != "InstanceTypeCatalog" {
		return nil, fmt.Errorf("expected kind InstanceTypeCatalog, got %s", catalog.Kind)
	}
	if len(catalog.InstanceTypes) == 0 {
		return nil, fmt.Errorf("instance types catalog is empty")
	}

	return catalog, nil
}

// ToEC2InstanceTypeInfos converts catalog instance types to EC2 SDK types.
func (c *InstanceTypeCatalog) ToEC2InstanceTypeInfos() []ec2types.InstanceTypeInfo {
	return lo.Map(c.InstanceTypes, func(it CatalogInstanceType, _ int) ec2types.InstanceTypeInfo {
		cpu := lo.Must(resource.ParseQuantity(it.CPU))
		mem := lo.Must(resource.ParseQuantity(it.Memory))

		// Determine architecture
		arch := ec2types.ArchitectureTypeX8664
		if it.Architecture == "arm64" {
			arch = ec2types.ArchitectureTypeArm64
		}

		return ec2types.InstanceTypeInfo{
			InstanceType: ec2types.InstanceType(it.Name),
			VCpuInfo: &ec2types.VCpuInfo{
				DefaultVCpus: aws.Int32(int32(cpu.Value())),
			},
			MemoryInfo: &ec2types.MemoryInfo{
				SizeInMiB: aws.Int64(mem.Value() / (1024 * 1024)),
			},
			ProcessorInfo: &ec2types.ProcessorInfo{
				SupportedArchitectures: []ec2types.ArchitectureType{arch},
			},
			NetworkInfo: &ec2types.NetworkInfo{
				MaximumNetworkInterfaces: aws.Int32(15),
				Ipv4AddressesPerInterface: aws.Int32(50),
			},
			SupportedUsageClasses: []ec2types.UsageClassType{
				ec2types.UsageClassTypeOnDemand,
				ec2types.UsageClassTypeSpot,
			},
			InstanceStorageSupported: aws.Bool(false),
			Hypervisor:               ec2types.InstanceTypeHypervisorNitro,
			CurrentGeneration:        aws.Bool(true),
		}
	})
}

// GetZones returns all unique zones from the catalog offerings, sorted alphabetically.
func (c *InstanceTypeCatalog) GetZones() []string {
	zones := make(map[string]struct{})
	for _, it := range c.InstanceTypes {
		for _, offering := range it.Offerings {
			zones[offering.Zone] = struct{}{}
		}
	}
	result := lo.Keys(zones)
	sort.Strings(result)
	return result
}

// GetPricing returns a map of instance type -> zone -> capacity type -> price.
func (c *InstanceTypeCatalog) GetPricing() map[string]map[string]map[string]float64 {
	pricing := make(map[string]map[string]map[string]float64)
	for _, it := range c.InstanceTypes {
		if pricing[it.Name] == nil {
			pricing[it.Name] = make(map[string]map[string]float64)
		}
		for _, offering := range it.Offerings {
			if pricing[it.Name][offering.Zone] == nil {
				pricing[it.Name][offering.Zone] = make(map[string]float64)
			}
			pricing[it.Name][offering.Zone][offering.CapacityType] = offering.Price
		}
	}
	return pricing
}

// GenerateFakeSubnets creates fake EC2 subnets for each zone in the catalog.
// This is useful for offline simulation where we don't have real AWS subnets.
func (c *InstanceTypeCatalog) GenerateFakeSubnets() []ec2types.Subnet {
	zones := c.GetZones()
	subnets := make([]ec2types.Subnet, len(zones))

	for i, zone := range zones {
		subnetID := fmt.Sprintf("subnet-fake-%s", zone)
		vpcID := "vpc-fake-simulation"
		cidr := fmt.Sprintf("10.%d.0.0/16", i)

		subnets[i] = ec2types.Subnet{
			SubnetId:                aws.String(subnetID),
			AvailabilityZone:        aws.String(zone),
			VpcId:                   aws.String(vpcID),
			CidrBlock:               aws.String(cidr),
			AvailableIpAddressCount: aws.Int32(65536),
			State:                   ec2types.SubnetStateAvailable,
			MapPublicIpOnLaunch:     aws.Bool(false),
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("simulation-%s", zone))},
				{Key: aws.String("karpenter.sh/discovery"), Value: aws.String("simulation")},
			},
		}
	}

	return subnets
}
