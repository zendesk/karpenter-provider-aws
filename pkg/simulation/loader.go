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

	"sigs.k8s.io/yaml"
)

// LoadSnapshot reads a ClusterSnapshot from a YAML file.
func LoadSnapshot(path string) (*ClusterSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading snapshot file: %w", err)
	}

	snapshot := &ClusterSnapshot{}
	if err := yaml.Unmarshal(data, snapshot); err != nil {
		return nil, fmt.Errorf("parsing snapshot YAML: %w", err)
	}

	// Validate required fields
	if snapshot.APIVersion == "" {
		return nil, fmt.Errorf("snapshot missing apiVersion")
	}
	if snapshot.Kind != "ClusterSnapshot" {
		return nil, fmt.Errorf("expected kind ClusterSnapshot, got %s", snapshot.Kind)
	}
	if snapshot.Metadata.ClusterName == "" {
		return nil, fmt.Errorf("snapshot missing metadata.clusterName")
	}

	return snapshot, nil
}

// SaveSnapshot writes a ClusterSnapshot to a YAML file.
func SaveSnapshot(snapshot *ClusterSnapshot, path string) error {
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing snapshot file: %w", err)
	}

	return nil
}

// ValidateSnapshot performs validation on a loaded snapshot.
func ValidateSnapshot(snapshot *ClusterSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("snapshot is nil")
	}

	if len(snapshot.InstanceTypes) == 0 {
		return fmt.Errorf("snapshot has no instance types")
	}

	// Validate instance types have required fields
	for i, it := range snapshot.InstanceTypes {
		if it.Name == "" {
			return fmt.Errorf("instanceType[%d] missing name", i)
		}
		if it.CPU == "" {
			return fmt.Errorf("instanceType[%d] (%s) missing cpu", i, it.Name)
		}
		if it.Memory == "" {
			return fmt.Errorf("instanceType[%d] (%s) missing memory", i, it.Name)
		}
	}

	// Validate pods have required fields
	for i, pod := range snapshot.Pods {
		if pod.Metadata.Name == "" {
			return fmt.Errorf("pod[%d] missing metadata.name", i)
		}
		if pod.Metadata.Namespace == "" {
			return fmt.Errorf("pod[%d] (%s) missing metadata.namespace", i, pod.Metadata.Name)
		}
	}

	// Validate nodes have required fields
	for i, node := range snapshot.Nodes {
		if node.Name == "" {
			return fmt.Errorf("node[%d] missing name", i)
		}
	}

	return nil
}
