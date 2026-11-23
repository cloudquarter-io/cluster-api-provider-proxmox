/*
Copyright 2023-2026 IONOS Cloud.

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

package scope

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMachineCodeReachesProxmoxThroughItsZone fails when code reaches Proxmox VE for a machine
// through the ProxmoxCluster's default client (machineScope.InfraCluster.ProxmoxClient) instead of
// MachineScope.ProxmoxClient. The default client reaches the ProxmoxCluster's own endpoint, not the
// Proxmox cluster of the machine's zone, and is nil when every zone carries its own credentialsRef.
// Code merged from upstream uses the default client, and it compiles, so this test finds it.
func TestMachineCodeReachesProxmoxThroughItsZone(t *testing.T) {
	var found []string
	for _, root := range []string{"../../internal", "../../pkg", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ProxmoxClient" {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "InfraCluster" {
					found = append(found, fset.Position(sel.Pos()).String())
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	require.Empty(t, found, "use MachineScope.ProxmoxClient(), which reaches the machine's zone")
}
