// Copyright 2021 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package driver

import (
	"fmt"
	"strings"
)

type PackageRegistry struct {
	packages map[string]*FlatPackage
	stdlib   map[string]string
}

func NewPackageRegistry(pkgs ...*FlatPackage) *PackageRegistry {
	pr := &PackageRegistry{
		packages: map[string]*FlatPackage{},
		stdlib:   map[string]string{},
	}
	pr.Add(pkgs...)
	return pr
}

const stdlibPrefix = "@@io_bazel_rules_go//stdlib:"

func canonicalizeId(path, id string, pkg *FlatPackage) string {
	if strings.HasPrefix(id, stdlibPrefix) {
		return id[len(stdlibPrefix):]
	}
	if pkg.IsStdlib() {
		return id
	}
	return path
}

func rewritePackage(pkg *FlatPackage) {
	pkg.ID = pkg.PkgPath
	for k, v := range pkg.Imports {
		// rewrite package ID mapping to be the same as the path
		pkg.Imports[k] = canonicalizeId(k, v, pkg)
	}
}

// returns true if a is a superset of b
func isSuperset(a, b []string) bool {
	if len(a) < len(b) {
		return false
	}
	bi := 0
	for i := range a {
		if a[i] == b[bi] {
			bi++
			if bi == len(b) {
				return true
			}
		}
	}
	return false
}

// Update merges the contents of 2 packages together in the instance where they have the same package path.
// This can happen when the gopackages aspect traverses to a child label and generates separate json files transitive targets.
// For example, in //proto/prysm/v1alpha1 we see both `:go_default_library` and `:go_proto` from `//proto/engine/v1`.
// Without the merge, `:go_proto` can overwrite `:go_default_library`, leaving sources files out of the final graph.
func (pr *PackageRegistry) Update(pkg *FlatPackage) {
	existing, ok := pr.packages[pkg.PkgPath]
	if !ok {
		pr.packages[pkg.PkgPath] = pkg
		return
	}
	if isSuperset(pkg.GoFiles, existing.GoFiles) {
		existing.GoFiles = pkg.GoFiles
	}
}

func (pr *PackageRegistry) Add(pkgs ...*FlatPackage) *PackageRegistry {
	for _, pkg := range pkgs {
		rewritePackage(pkg)
		pr.packages[pkg.PkgPath] = pkg

		if pkg.IsStdlib() {
			pr.stdlib[pkg.PkgPath] = pkg.ID
		}
	}
	return pr
}

func (pr *PackageRegistry) ResolvePaths(prf PathResolverFunc) error {
	for _, pkg := range pr.packages {
		pkg.ResolvePaths(prf)
		pkg.FilterFilesForBuildTags()
	}
	return nil
}

// ResolveImports adds stdlib imports to packages. This is required because
// stdlib packages are not part of the JSON file exports as bazel is unaware of
// them.
func (pr *PackageRegistry) ResolveImports() error {
	resolve := func(importPath string) string {
		if pkgID, ok := pr.stdlib[importPath]; ok {
			return pkgID
		}

		return ""
	}

	for _, pkg := range pr.packages {
		if err := pkg.ResolveImports(resolve); err != nil {
			return err
		}
		testFp := pkg.MoveTestFiles()
		if testFp != nil {
			pr.packages[testFp.ID] = testFp
		}
	}

	return nil
}

func (pr *PackageRegistry) walk(acc map[string]*FlatPackage, root string) {
	pkg := pr.packages[root]

	if pkg == nil {
		log.WithField("root", root).Error("package ID not found")
		return
	}

	acc[pkg.ID] = pkg
	for _, pkgID := range pkg.Imports {
		if _, ok := acc[pkgID]; !ok {
			pr.walk(acc, pkgID)
		}
	}
}

func (pr *PackageRegistry) Query(req *DriverRequest, queries []string) ([]string, []*FlatPackage) {
	walkedPackages := map[string]*FlatPackage{}
	retRoots := make([]string, 0, len(queries))
	for _, rootPkg := range queries {
		retRoots = append(retRoots, rootPkg)
		pr.walk(walkedPackages, rootPkg)
	}

	retPkgs := make([]*FlatPackage, 0, len(walkedPackages))
	for _, pkg := range walkedPackages {
		retPkgs = append(retPkgs, pkg)
	}

	return retRoots, retPkgs
}

func (pr *PackageRegistry) Match(labels []string) ([]string, []*FlatPackage) {
	roots := map[string]struct{}{}

	for _, label := range labels {
		// When packagesdriver is ran from rules go, rulesGoRepositoryName will just be @
		if !strings.HasPrefix(label, "@") {
			// Canonical labels is only since Bazel 6.0.0
			label = fmt.Sprintf("@%s", label)
		}

		if label == RulesGoStdlibLabel {
			// For stdlib, we need to append all the subpackages as roots
			// since RulesGoStdLibLabel doesn't actually show up in the stdlib pkg.json
			for _, pkg := range pr.packages {
				if pkg.Standard {
					roots[pkg.ID] = struct{}{}
				}
			}
		} else {
			roots[label] = struct{}{}
			// If an xtest package exists for this package add it to the roots
			if _, ok := pr.packages[label+"_xtest"]; ok {
				roots[label+"_xtest"] = struct{}{}
			}
		}
	}

	walkedPackages := map[string]*FlatPackage{}
	retRoots := make([]string, 0, len(roots))
	for rootPkg := range roots {
		retRoots = append(retRoots, rootPkg)
		pr.walk(walkedPackages, rootPkg)
	}

	retPkgs := make([]*FlatPackage, 0, len(walkedPackages))
	for _, pkg := range walkedPackages {
		retPkgs = append(retPkgs, pkg)
	}

	return retRoots, retPkgs
}
