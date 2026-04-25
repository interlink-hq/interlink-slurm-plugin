#!/usr/bin/env python3
"""
Patch for interLink Virtual Kubelet execute.go
Issue: https://github.com/interlink-hq/interlink-slurm-plugin/issues/144

When a projected volume ConfigMap source has no `items` list, the VK loop
  for _, item := range source.ConfigMap.Items { ... }
never executes, so no keys are projected. Kubernetes behaviour for an empty
items list is to project ALL keys from the ConfigMap.

This script adds that handling: when items is empty, fetch every key from the
ConfigMap and write it into the projected-volume data map.
"""

import sys
import os

if len(sys.argv) != 2:
    print(f"Usage: {sys.argv[0]} <path/to/execute.go>", file=sys.stderr)
    sys.exit(1)

target_file = sys.argv[1]

with open(target_file, "r") as f:
    content = f.read()

# The exact string we are looking for (tabs as used in the Go source)
OLD = "\t\tfor _, item := range source.ConfigMap.Items {"

NEW = """\t\t// When no items are specified, project ALL keys from the ConfigMap
\t\t// (standard Kubernetes behaviour for projected ConfigMap sources).
\t\tif len(source.ConfigMap.Items) == 0 {
\t\t\tcfgmap, err := p.clientSet.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, source.ConfigMap.Name, metav1.GetOptions{})
\t\t\tif err != nil {
\t\t\t\treturn fmt.Errorf("error during retrieval of ConfigMap %s error: %w", source.ConfigMap.Name, err)
\t\t\t}
\t\t\tfor k, v := range cfgmap.Data {
\t\t\t\tprojectedVolume.Data[k] = v
\t\t\t}
\t\t}
\t\tfor _, item := range source.ConfigMap.Items {"""

if OLD not in content:
    print(f"ERROR: target string not found in {target_file}", file=sys.stderr)
    print("The VK source may have changed; patch needs updating.", file=sys.stderr)
    sys.exit(1)

count = content.count(OLD)
if count != 1:
    print(f"ERROR: expected exactly 1 occurrence, found {count}", file=sys.stderr)
    sys.exit(1)

patched = content.replace(OLD, NEW, 1)

with open(target_file, "w") as f:
    f.write(patched)

print(f"✓ Patch applied to {target_file}")
