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

// instancedata fetches AWS instance type information for offline simulation.
// This tool retrieves instance types, their capacities, and pricing from AWS APIs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
	"gopkg.in/yaml.v3"

	"github.com/aws/karpenter-provider-aws/pkg/simulation"
)

var (
	outputFile   string
	region       string
	zones        string
	families     string
	excludeFam   string
	onlyOnDemand bool
	noPricing    bool
)

func init() {
	flag.StringVar(&outputFile, "output", "instancetypes.yaml", "output file path")
	flag.StringVar(&region, "region", "", "AWS region (default: from AWS_REGION or config)")
	flag.StringVar(&zones, "zones", "", "specific zones to include (comma-separated, empty = all)")
	flag.StringVar(&families, "families", "", "instance families to include (comma-separated, empty = all)")
	flag.StringVar(&excludeFam, "exclude-families", "", "instance families to exclude (comma-separated)")
	flag.BoolVar(&onlyOnDemand, "on-demand-only", false, "only include on-demand pricing")
	flag.BoolVar(&noPricing, "no-pricing", false, "skip pricing lookup (faster, but no prices)")
	flag.Parse()
}

// InstanceTypeCatalog is the output format for instance type data
type InstanceTypeCatalog struct {
	APIVersion string                            `yaml:"apiVersion"`
	Kind       string                            `yaml:"kind"`
	Metadata   CatalogMetadata                   `yaml:"metadata"`
	Types      []simulation.InstanceTypeSnapshot `yaml:"instanceTypes"`
}

type CatalogMetadata struct {
	CapturedAt time.Time `yaml:"capturedAt"`
	Region     string    `yaml:"region"`
	Zones      []string  `yaml:"zones,omitempty"`
	Count      int       `yaml:"count"`
}

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	slog.Info("fetching instance type data", "output", outputFile)

	// Load AWS config
	var cfgOpts []func(*config.LoadOptions) error
	if region != "" {
		cfgOpts = append(cfgOpts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		slog.Error("failed to load AWS config", "error", err)
		os.Exit(1)
	}

	resolvedRegion := cfg.Region
	if resolvedRegion == "" {
		slog.Error("no region specified, use --region or AWS_REGION")
		os.Exit(1)
	}
	slog.Info("using region", "region", resolvedRegion)

	ec2Client := ec2.NewFromConfig(cfg)

	// Fetch availability zones
	availableZones, err := fetchZones(ctx, ec2Client)
	if err != nil {
		slog.Error("failed to fetch zones", "error", err)
		os.Exit(1)
	}

	// Filter zones if specified
	if zones != "" {
		requestedZones := parseCommaSeparated(zones)
		availableZones = lo.Filter(availableZones, func(z string, _ int) bool {
			return lo.Contains(requestedZones, z)
		})
	}
	slog.Info("zones", "count", len(availableZones), "zones", availableZones)

	// Fetch instance types
	instanceTypes, err := fetchInstanceTypes(ctx, ec2Client)
	if err != nil {
		slog.Error("failed to fetch instance types", "error", err)
		os.Exit(1)
	}
	slog.Info("fetched instance types", "count", len(instanceTypes))

	// Filter by family if specified
	includeFamilies := parseCommaSeparated(families)
	excludeFamilies := parseCommaSeparated(excludeFam)

	instanceTypes = lo.Filter(instanceTypes, func(it ec2types.InstanceTypeInfo, _ int) bool {
		family := getFamily(string(it.InstanceType))

		// Check include list
		if len(includeFamilies) > 0 && !lo.Contains(includeFamilies, family) {
			return false
		}

		// Check exclude list
		if len(excludeFamilies) > 0 && lo.Contains(excludeFamilies, family) {
			return false
		}

		return true
	})
	slog.Info("filtered instance types", "count", len(instanceTypes))

	// Fetch instance type offerings (zone availability)
	offerings, err := fetchOfferings(ctx, ec2Client, availableZones)
	if err != nil {
		slog.Error("failed to fetch offerings", "error", err)
		os.Exit(1)
	}
	slog.Info("fetched offerings", "count", len(offerings))

	// Convert to snapshot format
	snapshots := convertToSnapshots(instanceTypes, offerings, availableZones, noPricing, onlyOnDemand, resolvedRegion)

	// Sort by name
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Name < snapshots[j].Name
	})

	// Build catalog
	catalog := InstanceTypeCatalog{
		APIVersion: "simulation.karpenter.sh/v1alpha1",
		Kind:       "InstanceTypeCatalog",
		Metadata: CatalogMetadata{
			CapturedAt: time.Now(),
			Region:     resolvedRegion,
			Zones:      availableZones,
			Count:      len(snapshots),
		},
		Types: snapshots,
	}

	// Write output
	f, err := os.Create(outputFile)
	if err != nil {
		slog.Error("failed to create output file", "error", err)
		os.Exit(1)
	}
	defer f.Close()

	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	if err := enc.Encode(catalog); err != nil {
		slog.Error("failed to encode YAML", "error", err)
		os.Exit(1)
	}

	slog.Info("instance type data saved",
		"output", outputFile,
		"count", len(snapshots),
		"region", resolvedRegion,
	)
}

func fetchZones(ctx context.Context, client *ec2.Client) ([]string, error) {
	resp, err := client.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{
			{
				Name:   lo.ToPtr("state"),
				Values: []string{"available"},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	zones := make([]string, 0, len(resp.AvailabilityZones))
	for _, az := range resp.AvailabilityZones {
		if az.ZoneName != nil {
			zones = append(zones, *az.ZoneName)
		}
	}
	sort.Strings(zones)
	return zones, nil
}

func fetchInstanceTypes(ctx context.Context, client *ec2.Client) ([]ec2types.InstanceTypeInfo, error) {
	var instanceTypes []ec2types.InstanceTypeInfo

	paginator := ec2.NewDescribeInstanceTypesPaginator(client, &ec2.DescribeInstanceTypesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		instanceTypes = append(instanceTypes, page.InstanceTypes...)
	}

	return instanceTypes, nil
}

func fetchOfferings(ctx context.Context, client *ec2.Client, zones []string) (map[string]map[string]bool, error) {
	// Map of instance type -> zone -> available
	offerings := make(map[string]map[string]bool)

	// Fetch on-demand offerings
	paginator := ec2.NewDescribeInstanceTypeOfferingsPaginator(client, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{
			{
				Name:   lo.ToPtr("location"),
				Values: zones,
			},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, offering := range page.InstanceTypeOfferings {
			it := string(offering.InstanceType)
			zone := lo.FromPtr(offering.Location)

			if offerings[it] == nil {
				offerings[it] = make(map[string]bool)
			}
			offerings[it][zone] = true
		}
	}

	return offerings, nil
}

func convertToSnapshots(
	instanceTypes []ec2types.InstanceTypeInfo,
	offerings map[string]map[string]bool,
	zones []string,
	noPricing bool,
	onlyOnDemand bool,
	region string,
) []simulation.InstanceTypeSnapshot {
	var snapshots []simulation.InstanceTypeSnapshot

	for _, it := range instanceTypes {
		name := string(it.InstanceType)

		// Get architecture
		arch := "amd64"
		if it.ProcessorInfo != nil && len(it.ProcessorInfo.SupportedArchitectures) > 0 {
			for _, a := range it.ProcessorInfo.SupportedArchitectures {
				if a == ec2types.ArchitectureTypeArm64 {
					arch = "arm64"
					break
				}
			}
		}

		// Get CPU and memory
		var cpu, memory string
		if it.VCpuInfo != nil && it.VCpuInfo.DefaultVCpus != nil {
			cpu = fmt.Sprintf("%d", *it.VCpuInfo.DefaultVCpus)
		}
		if it.MemoryInfo != nil && it.MemoryInfo.SizeInMiB != nil {
			// Convert MiB to Kubernetes format
			memory = fmt.Sprintf("%dMi", *it.MemoryInfo.SizeInMiB)
		}

		// Build offerings for each zone
		var snapshotOfferings []simulation.OfferingSnapshot
		zoneOfferings := offerings[name]

		for _, zone := range zones {
			available := zoneOfferings[zone]

			// On-demand offering
			snapshotOfferings = append(snapshotOfferings, simulation.OfferingSnapshot{
				Zone:         zone,
				CapacityType: "on-demand",
				Price:        getPriceOnDemand(name, region, noPricing),
				Available:    available,
			})

			// Spot offering (if not on-demand only)
			if !onlyOnDemand {
				snapshotOfferings = append(snapshotOfferings, simulation.OfferingSnapshot{
					Zone:         zone,
					CapacityType: "spot",
					Price:        getPriceSpot(name, region, noPricing),
					Available:    available,
				})
			}
		}

		// Build capacity info
		capacity := simulation.ResourceSnapshot{}
		if it.InstanceStorageInfo != nil && it.InstanceStorageInfo.TotalSizeInGB != nil {
			capacity.EphemeralStorage = fmt.Sprintf("%dGi", *it.InstanceStorageInfo.TotalSizeInGB)
		}
		if it.NetworkInfo != nil && it.NetworkInfo.MaximumNetworkInterfaces != nil {
			// Calculate max pods from ENI limits
			// Formula: (ENIs * (IPs per ENI - 1)) + 2
			maxENIs := int32(0)
			maxIPsPerENI := int32(0)
			if it.NetworkInfo.MaximumNetworkInterfaces != nil {
				maxENIs = *it.NetworkInfo.MaximumNetworkInterfaces
			}
			if it.NetworkInfo.Ipv4AddressesPerInterface != nil {
				maxIPsPerENI = *it.NetworkInfo.Ipv4AddressesPerInterface
			}
			if maxENIs > 0 && maxIPsPerENI > 0 {
				maxPods := (maxENIs * (maxIPsPerENI - 1)) + 2
				capacity.Pods = fmt.Sprintf("%d", maxPods)
			}
		}

		// GPU info
		if it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0 {
			totalGPUs := int32(0)
			for _, gpu := range it.GpuInfo.Gpus {
				if gpu.Count != nil {
					totalGPUs += *gpu.Count
				}
			}
			if totalGPUs > 0 {
				// Determine manufacturer
				if len(it.GpuInfo.Gpus) > 0 && it.GpuInfo.Gpus[0].Manufacturer != nil {
					mfr := strings.ToLower(*it.GpuInfo.Gpus[0].Manufacturer)
					if strings.Contains(mfr, "nvidia") {
						capacity.NvidiaGPU = fmt.Sprintf("%d", totalGPUs)
					} else if strings.Contains(mfr, "amd") {
						capacity.AMDGPU = fmt.Sprintf("%d", totalGPUs)
					}
				}
			}
		}

		// Neuron info (Inferentia/Trainium)
		if it.InferenceAcceleratorInfo != nil && len(it.InferenceAcceleratorInfo.Accelerators) > 0 {
			totalNeurons := int32(0)
			for _, acc := range it.InferenceAcceleratorInfo.Accelerators {
				if acc.Count != nil {
					totalNeurons += *acc.Count
				}
			}
			if totalNeurons > 0 {
				capacity.AWSNeuron = fmt.Sprintf("%d", totalNeurons)
			}
		}

		// EFA
		if it.NetworkInfo != nil && it.NetworkInfo.EfaSupported != nil && *it.NetworkInfo.EfaSupported {
			if it.NetworkInfo.EfaInfo != nil && it.NetworkInfo.EfaInfo.MaximumEfaInterfaces != nil {
				capacity.EFA = fmt.Sprintf("%d", *it.NetworkInfo.EfaInfo.MaximumEfaInterfaces)
			} else {
				capacity.EFA = "1"
			}
		}

		snapshots = append(snapshots, simulation.InstanceTypeSnapshot{
			Name:         name,
			CPU:          cpu,
			Memory:       memory,
			Architecture: arch,
			Offerings:    snapshotOfferings,
			Capacity:     capacity,
		})
	}

	return snapshots
}

// getPriceOnDemand returns on-demand price. Currently returns 0 if noPricing is true,
// otherwise returns a placeholder. In production, this should use the Pricing API.
func getPriceOnDemand(instanceType, region string, noPricing bool) float64 {
	if noPricing {
		return 0
	}
	// TODO: Implement actual pricing lookup using AWS Pricing API
	// For now, use the pre-generated pricing data from pkg/providers/pricing
	return getEstimatedPrice(instanceType, region, "on-demand")
}

// getPriceSpot returns spot price estimate.
func getPriceSpot(instanceType, region string, noPricing bool) float64 {
	if noPricing {
		return 0
	}
	// Spot is typically ~60-70% of on-demand
	return getEstimatedPrice(instanceType, region, "spot")
}

// getEstimatedPrice returns an estimated price based on instance size.
// This is a rough estimate - real pricing should use AWS Pricing API.
func getEstimatedPrice(instanceType, region, capacityType string) float64 {
	// Parse instance type to get family and size
	parts := strings.Split(instanceType, ".")
	if len(parts) != 2 {
		return 0
	}
	size := parts[1]

	// Base prices vary by family, but we'll use a simple size-based estimate
	// These are rough US-region on-demand prices
	basePrice := map[string]float64{
		"nano":     0.0042,
		"micro":    0.0084,
		"small":    0.0168,
		"medium":   0.0336,
		"large":    0.0672,
		"xlarge":   0.1344,
		"2xlarge":  0.2688,
		"4xlarge":  0.5376,
		"8xlarge":  1.0752,
		"12xlarge": 1.6128,
		"16xlarge": 2.1504,
		"24xlarge": 3.2256,
		"metal":    3.2256,
	}

	price := basePrice[size]
	if price == 0 {
		// Handle sizes like "9xlarge", "18xlarge", etc.
		if strings.HasSuffix(size, "xlarge") {
			sizeNum := strings.TrimSuffix(size, "xlarge")
			if sizeNum != "" {
				// Parse the number
				var n int
				fmt.Sscanf(sizeNum, "%d", &n)
				price = 0.1344 * float64(n) // xlarge base * multiplier
			}
		}
	}

	// Spot discount
	if capacityType == "spot" {
		price *= 0.65 // ~35% discount
	}

	return price
}

func getFamily(instanceType string) string {
	parts := strings.Split(instanceType, ".")
	if len(parts) != 2 {
		return instanceType
	}
	return parts[0]
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
