# Plan: Sanitize TopologySpreadConstraints for Simulated Pods

## Problem

When creating pods in the simulated KWOK cluster from a snapshot, Kubernetes rejects pods with this error:

```
Pod "..." is invalid: spec.topologySpreadConstraints[0].matchLabelKeys[0]: Invalid value: "pod-template-hash": exists in both matchLabelKeys and labelSelector
```

This occurs because pods from the real cluster have `topologySpreadConstraints` where the same label key (`pod-template-hash`) appears in both:
- `matchLabelKeys` (dynamic label matching)
- `labelSelector.matchLabels` (static label selector)

This is a Kubernetes API validation rule (introduced in K8s 1.27+) that prevents redundant/conflicting configuration.

## Root Cause

The `ToCorePod()` method in `pkg/simulation/types.go` copies `TopologySpreadConstraints` directly from the snapshot without sanitizing them. In production, these pods were created by controllers that may have set both fields, which was valid in older Kubernetes versions or under different validation rules.

## Solution Options

### Option A: Sanitize in `ToCorePod()` method (Recommended)

Add a helper function to clean up `TopologySpreadConstraints` by removing keys from `matchLabelKeys` that already exist in `labelSelector`.

**Pros:**
- Fixes the issue at the conversion layer
- Preserves the intended behavior (spreading by topology)
- Transparent to callers
- Existing snapshots work without modification

**Cons:**
- Slightly modifies pod spec from original

### Option B: Sanitize during snapshot capture

Clean up the constraints when capturing the snapshot in `cmd/snapshot/main.go`.

**Pros:**
- Snapshots are "clean" and portable

**Cons:**
- Modifies the snapshot data (loses fidelity to original cluster)
- Existing snapshots would still have the issue
- Requires re-capturing snapshots

### Option C: Clear `matchLabelKeys` entirely

Remove `matchLabelKeys` from all constraints during pod creation.

**Pros:**
- Simplest implementation

**Cons:**
- Loses scheduling nuance that `matchLabelKeys` provides
- May affect simulation accuracy for workloads that rely on this feature

## Implementation Plan (Option A)

### Step 1: Add helper function

Add `sanitizeTopologySpreadConstraints()` in `pkg/simulation/types.go`:

```go
// sanitizeTopologySpreadConstraints removes matchLabelKeys entries that
// already exist in the labelSelector to avoid K8s validation errors.
// This is necessary because K8s 1.27+ rejects TSCs where the same key
// appears in both matchLabelKeys and labelSelector.
func sanitizeTopologySpreadConstraints(constraints []corev1.TopologySpreadConstraint) []corev1.TopologySpreadConstraint {
    if len(constraints) == 0 {
        return constraints
    }

    result := make([]corev1.TopologySpreadConstraint, len(constraints))
    for i, tsc := range constraints {
        result[i] = *tsc.DeepCopy()

        if result[i].LabelSelector == nil || len(result[i].MatchLabelKeys) == 0 {
            continue
        }

        // Collect keys from labelSelector
        selectorKeys := make(map[string]bool)
        for k := range result[i].LabelSelector.MatchLabels {
            selectorKeys[k] = true
        }
        for _, expr := range result[i].LabelSelector.MatchExpressions {
            selectorKeys[expr.Key] = true
        }

        // Filter matchLabelKeys to remove duplicates
        var filteredKeys []string
        for _, key := range result[i].MatchLabelKeys {
            if !selectorKeys[key] {
                filteredKeys = append(filteredKeys, key)
            }
        }
        result[i].MatchLabelKeys = filteredKeys
    }
    return result
}
```

### Step 2: Update `ToCorePod()`

Modify the `ToCorePod()` method to call the sanitizer:

```go
func (p *PodSnapshot) ToCorePod() *corev1.Pod {
    // ... existing container conversion code ...

    return &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:            p.Metadata.Name,
            Namespace:       p.Metadata.Namespace,
            Labels:          p.Metadata.Labels,
            Annotations:     p.Metadata.Annotations,
            OwnerReferences: p.Metadata.OwnerReferences,
        },
        Spec: corev1.PodSpec{
            Containers:                containers,
            InitContainers:            initContainers,
            NodeSelector:              p.Spec.NodeSelector,
            Affinity:                  p.Spec.Affinity,
            TopologySpreadConstraints: sanitizeTopologySpreadConstraints(p.Spec.TopologySpreadConstraints),
            Tolerations:               p.Spec.Tolerations,
            PriorityClassName:         p.Spec.PriorityClassName,
            Priority:                  p.Spec.Priority,
            RuntimeClassName:          p.Spec.RuntimeClassName,
        },
    }
}
```

### Step 3: Add tests (optional but recommended)

Add unit tests for `sanitizeTopologySpreadConstraints()`:

```go
func TestSanitizeTopologySpreadConstraints(t *testing.T) {
    tests := []struct {
        name     string
        input    []corev1.TopologySpreadConstraint
        expected []corev1.TopologySpreadConstraint
    }{
        {
            name:     "empty constraints",
            input:    nil,
            expected: nil,
        },
        {
            name: "no overlap",
            input: []corev1.TopologySpreadConstraint{{
                TopologyKey: "topology.kubernetes.io/zone",
                LabelSelector: &metav1.LabelSelector{
                    MatchLabels: map[string]string{"app": "test"},
                },
                MatchLabelKeys: []string{"pod-template-hash"},
            }},
            expected: []corev1.TopologySpreadConstraint{{
                TopologyKey: "topology.kubernetes.io/zone",
                LabelSelector: &metav1.LabelSelector{
                    MatchLabels: map[string]string{"app": "test"},
                },
                MatchLabelKeys: []string{"pod-template-hash"},
            }},
        },
        {
            name: "overlap in matchLabels",
            input: []corev1.TopologySpreadConstraint{{
                TopologyKey: "topology.kubernetes.io/zone",
                LabelSelector: &metav1.LabelSelector{
                    MatchLabels: map[string]string{
                        "app":               "test",
                        "pod-template-hash": "abc123",
                    },
                },
                MatchLabelKeys: []string{"pod-template-hash"},
            }},
            expected: []corev1.TopologySpreadConstraint{{
                TopologyKey: "topology.kubernetes.io/zone",
                LabelSelector: &metav1.LabelSelector{
                    MatchLabels: map[string]string{
                        "app":               "test",
                        "pod-template-hash": "abc123",
                    },
                },
                MatchLabelKeys: []string{}, // filtered out
            }},
        },
    }
    // ... test implementation ...
}
```

## Testing

1. **Build the simulator:**
   ```bash
   go build -o simulator ./cmd/simulator/...
   ```

2. **Run with existing snapshot:**
   ```bash
   ./simulate.sh example ./sandbox.yml
   ```

3. **Verify pods are created:**
   ```bash
   export KUBECONFIG=./kwok-example.kubeconfig
   kubectl get pods -A | grep -v "Running\|Completed"
   ```

4. **Check for remaining validation errors:**
   ```bash
   kubectl get events --field-selector reason=FailedCreate
   ```

## Files to Modify

- [x] `pkg/simulation/types.go` - Add sanitization function and update `ToCorePod()`
- [x] `pkg/simulation/types_test.go` (new) - Add unit tests

## Additional Considerations

- **Logging**: Consider adding a debug log when keys are removed to aid troubleshooting
- **Documentation**: Update any simulation docs to note that pod specs may be slightly modified
- **Future validation**: Watch for other K8s validation rules that may affect simulation fidelity
