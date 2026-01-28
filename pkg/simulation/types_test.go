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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSanitizeTopologySpreadConstraints(t *testing.T) {
	tests := []struct {
		name                    string
		input                   []corev1.TopologySpreadConstraint
		expectedMatchLabelKeys  [][]string // expected matchLabelKeys for each constraint
		expectedConstraintCount int
	}{
		{
			name:                    "nil constraints",
			input:                   nil,
			expectedMatchLabelKeys:  nil,
			expectedConstraintCount: 0,
		},
		{
			name:                    "empty constraints",
			input:                   []corev1.TopologySpreadConstraint{},
			expectedMatchLabelKeys:  [][]string{},
			expectedConstraintCount: 0,
		},
		{
			name: "no overlap - matchLabelKeys preserved",
			input: []corev1.TopologySpreadConstraint{{
				TopologyKey:       "topology.kubernetes.io/zone",
				MaxSkew:           1,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
				MatchLabelKeys: []string{"pod-template-hash"},
			}},
			expectedMatchLabelKeys:  [][]string{{"pod-template-hash"}},
			expectedConstraintCount: 1,
		},
		{
			name: "overlap in matchLabels - key removed",
			input: []corev1.TopologySpreadConstraint{{
				TopologyKey:       "topology.kubernetes.io/zone",
				MaxSkew:           1,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":               "test",
						"pod-template-hash": "abc123",
					},
				},
				MatchLabelKeys: []string{"pod-template-hash"},
			}},
			expectedMatchLabelKeys:  [][]string{nil}, // filtered to empty
			expectedConstraintCount: 1,
		},
		{
			name: "overlap in matchExpressions - key removed",
			input: []corev1.TopologySpreadConstraint{{
				TopologyKey:       "topology.kubernetes.io/zone",
				MaxSkew:           1,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "pod-template-hash",
						Operator: metav1.LabelSelectorOpExists,
					}},
				},
				MatchLabelKeys: []string{"pod-template-hash"},
			}},
			expectedMatchLabelKeys:  [][]string{nil}, // filtered to empty
			expectedConstraintCount: 1,
		},
		{
			name: "partial overlap - only overlapping key removed",
			input: []corev1.TopologySpreadConstraint{{
				TopologyKey:       "topology.kubernetes.io/zone",
				MaxSkew:           1,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":               "test",
						"pod-template-hash": "abc123",
					},
				},
				MatchLabelKeys: []string{"pod-template-hash", "controller-revision-hash"},
			}},
			expectedMatchLabelKeys:  [][]string{{"controller-revision-hash"}},
			expectedConstraintCount: 1,
		},
		{
			name: "no labelSelector - matchLabelKeys preserved",
			input: []corev1.TopologySpreadConstraint{{
				TopologyKey:       "topology.kubernetes.io/zone",
				MaxSkew:           1,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector:     nil,
				MatchLabelKeys:    []string{"pod-template-hash"},
			}},
			expectedMatchLabelKeys:  [][]string{{"pod-template-hash"}},
			expectedConstraintCount: 1,
		},
		{
			name: "no matchLabelKeys - constraint unchanged",
			input: []corev1.TopologySpreadConstraint{{
				TopologyKey:       "topology.kubernetes.io/zone",
				MaxSkew:           1,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
				MatchLabelKeys: nil,
			}},
			expectedMatchLabelKeys:  [][]string{nil},
			expectedConstraintCount: 1,
		},
		{
			name: "multiple constraints - each sanitized independently",
			input: []corev1.TopologySpreadConstraint{
				{
					TopologyKey:       "topology.kubernetes.io/zone",
					MaxSkew:           1,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app":               "test",
							"pod-template-hash": "abc123",
						},
					},
					MatchLabelKeys: []string{"pod-template-hash"},
				},
				{
					TopologyKey:       "kubernetes.io/hostname",
					MaxSkew:           1,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					MatchLabelKeys: []string{"pod-template-hash"},
				},
			},
			expectedMatchLabelKeys:  [][]string{nil, {"pod-template-hash"}},
			expectedConstraintCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeTopologySpreadConstraints(tt.input)

			// Check constraint count
			if len(result) != tt.expectedConstraintCount {
				t.Errorf("expected %d constraints, got %d", tt.expectedConstraintCount, len(result))
				return
			}

			// Check matchLabelKeys for each constraint
			for i, expected := range tt.expectedMatchLabelKeys {
				got := result[i].MatchLabelKeys
				if !stringSlicesEqual(expected, got) {
					t.Errorf("constraint[%d]: expected matchLabelKeys %v, got %v", i, expected, got)
				}
			}
		})
	}
}

func TestSanitizeTopologySpreadConstraints_DoesNotMutateInput(t *testing.T) {
	original := []corev1.TopologySpreadConstraint{{
		TopologyKey:       "topology.kubernetes.io/zone",
		MaxSkew:           1,
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app":               "test",
				"pod-template-hash": "abc123",
			},
		},
		MatchLabelKeys: []string{"pod-template-hash"},
	}}

	// Copy the original matchLabelKeys to verify it doesn't change
	originalKeys := make([]string, len(original[0].MatchLabelKeys))
	copy(originalKeys, original[0].MatchLabelKeys)

	_ = sanitizeTopologySpreadConstraints(original)

	// Verify original was not mutated
	if !stringSlicesEqual(original[0].MatchLabelKeys, originalKeys) {
		t.Errorf("input was mutated: original matchLabelKeys changed from %v to %v",
			originalKeys, original[0].MatchLabelKeys)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
