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

package strategy

import (
	"fmt"
	"math"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/aws/karpenter-provider-aws/pkg/simulation"
)

type Strategy interface {
	GetScore(instanceType, capacityType, availabilityZone string) float64
}

type LowestPrice struct {
	// pricing is a map of instance type -> zone -> capacity type -> price
	pricing map[string]map[string]map[string]float64
}

// NewLowestPriceFromCatalog creates a LowestPrice strategy using offline pricing data from an instance type catalog.
func NewLowestPriceFromCatalog(catalog *simulation.InstanceTypeCatalog) *LowestPrice {
	return &LowestPrice{
		pricing: catalog.GetPricing(),
	}
}

func (p *LowestPrice) GetScore(instanceType, capacityType, availabilityZone string) float64 {
	zones, ok := p.pricing[instanceType]
	if !ok {
		return math.MaxFloat64
	}

	caps, ok := zones[availabilityZone]
	if !ok {
		return math.MaxFloat64
	}

	switch capacityType {
	case v1.CapacityTypeSpot:
		if price, ok := caps["spot"]; ok {
			return price
		}
		return math.MaxFloat64
	case v1.CapacityTypeOnDemand:
		if price, ok := caps["on-demand"]; ok {
			return price
		}
		return math.MaxFloat64
	default:
		panic(fmt.Sprintf("Unsupported capacity type: %s", capacityType))
	}
}
