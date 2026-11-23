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
	"go/types"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	modulePath       = "github.com/ionos-cloud/cluster-api-provider-proxmox"
	scopePackagePath = modulePath + "/pkg/scope"
	clientPackage    = modulePath + "/pkg/proxmox"
	// clusterScopeFile holds the ClusterScope, which owns the default client.
	clusterScopeFile = "cluster.go"
)

// TestMachineCodeReachesProxmoxThroughItsZone fails when production code reads a ProxmoxClient field
// holding the ProxmoxCluster's default client (ClusterScope.ProxmoxClient, a reconciler's
// ProxmoxClient, ...) for any use but handing it to ClusterScopeParams. Only the ClusterScope's own
// file, which owns the field, may read it; MachineScope code is checked too. The default client
// reaches the ProxmoxCluster's own endpoint, not the Proxmox cluster of a machine's zone, and is nil
// when every zone carries its own credentialsRef. Machine code uses MachineScope.ProxmoxClient().
// Code merged from upstream reads the field, and it compiles, so this test finds it by its type, not
// by its spelling.
func TestMachineCodeReachesProxmoxThroughItsZone(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  "../..",
	}, "./...")
	require.NoError(t, err)

	var found []string
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			t.Errorf("%s: %s", pkg.PkgPath, e)
		}
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			if pkg.PkgPath == scopePackagePath && filepath.Base(pkg.Fset.Position(file.Pos()).Filename) == clusterScopeFile {
				continue
			}
			handedToScope := map[ast.Expr]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isClusterScopeParams(pkg.TypesInfo.TypeOf(lit)) {
					return true
				}
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ProxmoxClient" {
							handedToScope[kv.Value] = true
						}
					}
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || handedToScope[sel] {
					return true
				}
				if s := pkg.TypesInfo.Selections[sel]; s != nil && s.Kind() == types.FieldVal &&
					s.Obj().Name() == "ProxmoxClient" && isProxmoxClient(s.Type()) {
					found = append(found, pkg.Fset.Position(sel.Pos()).String())
				}
				return true
			})
		}
	}
	require.Empty(t, found, "use MachineScope.ProxmoxClient(), which reaches the machine's zone")
}

func isClusterScopeParams(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == scopePackagePath &&
		named.Obj().Name() == "ClusterScopeParams"
}

func isProxmoxClient(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == clientPackage &&
		named.Obj().Name() == "Client"
}
