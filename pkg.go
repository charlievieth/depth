package depth

import (
	"go/build"
	"path"
	"sort"
	"strings"
	"sync"
)

// Pkg represents a Go source package, and its dependencies.
type Pkg struct {
	mu     sync.Mutex
	Name   string `json:"name"`
	SrcDir string `json:"-"`

	Internal bool `json:"internal"`
	Resolved bool `json:"resolved"`
	Test     bool `json:"-"`

	Tree   *Tree `json:"-"`
	Parent *Pkg  `json:"-"`
	Deps   []Pkg `json:"deps"`

	Raw *build.Package `json:"-"`
}

type stringSet struct {
	mu  sync.Mutex
	set map[string]struct{}
}

func (s *stringSet) Reset() {
	s.mu.Lock()
	if s.set != nil {
		for k := range s.set {
			delete(s.set, k)
		}
	}
	s.mu.Unlock()
}

func (s *stringSet) Contains(str string) bool {
	s.mu.Lock()
	_, seen := s.set[str]
	s.mu.Unlock()
	return seen
}

func (s *stringSet) Add(str string) (added bool) {
	s.mu.Lock()
	if s.set == nil {
		s.set = make(map[string]struct{})
	}
	if _, ok := s.set[str]; !ok {
		added = true
		s.set[str] = struct{}{}
	}
	s.mu.Unlock()
	return added
}

// Resolve recursively finds all dependencies for the Pkg and the packages it depends on.
func (p *Pkg) Resolve(i Importer) {
	// Resolved is always true, regardless of if we skip the import,
	// it is only false if there is an error while importing.
	p.Resolved = true

	name := p.cleanName()
	if name == "" {
		return
	}

	// Stop resolving imports if we've reached max depth or found a duplicate.
	var importMode build.ImportMode
	if p.Tree.hasSeenImport(name) || p.Tree.isAtMaxDepth(p) {
		importMode = build.FindOnly
	}

	pkg, err := i.Import(name, p.SrcDir, importMode)
	if err != nil {
		// TODO: Check the error type?
		p.Resolved = false
		return
	}
	p.Raw = pkg

	// Update the name with the fully qualified import path.
	p.Name = pkg.ImportPath

	// If this is an internal dependency, we may need to skip it.
	if pkg.Goroot {
		p.Internal = true
		if !p.Tree.shouldResolveInternal(p) {
			return
		}
	}

	//first we set the regular dependencies, then we add the test dependencies
	//sharing the same set. This allows us to mark all test-only deps linearly
	var unique stringSet
	p.setDeps(i, pkg.Imports, pkg.Dir, &unique, false)
	if p.Tree.ResolveTest {
		p.setDeps(i, append(pkg.TestImports, pkg.XTestImports...), pkg.Dir, &unique, true)
	}
}

// setDeps takes a slice of import paths and the source directory they are relative to,
// and creates the Deps of the Pkg. Each dependency is also further resolved prior to being added
// to the Pkg.
func (p *Pkg) setDeps(i Importer, imports []string, srcDir string, unique *stringSet, isTest bool) {
	var wg sync.WaitGroup
	for _, imp := range imports {
		// Mostly for testing files where cyclic imports are allowed.
		if imp == p.Name {
			continue
		}
		// Skip duplicates.
		if !unique.Add(imp) {
			continue
		}
		wg.Add(1)
		go func(imp string) {
			// TODO: limit number of goroutines (NOTE: this func is recursively called)
			defer wg.Done()
			p.addDep(i, imp, srcDir, isTest)
		}(imp)
	}
	wg.Wait()

	sort.Sort(byInternalAndName(p.Deps))
}

func (p *Pkg) appendDep(dep Pkg) {
	p.mu.Lock()
	p.Deps = append(p.Deps, dep)
	p.mu.Unlock()
}

// addDep creates a Pkg and it's dependencies from an imported package name.
func (p *Pkg) addDep(i Importer, name, srcDir string, isTest bool) {
	dep := Pkg{
		Name:   name,
		SrcDir: srcDir,
		Tree:   p.Tree,
		Parent: p,
		Test:   isTest,
	}
	dep.Resolve(i)
	p.appendDep(dep)

	// p.Deps = append(p.Deps, dep)
}

// depth returns the depth of the Pkg within the Tree.
func (p *Pkg) depth() int {
	n := 0
	for pp := p.Parent; pp != nil; pp = pp.Parent {
		n++
	}
	return n
}

// cleanName returns a cleaned version of the Pkg name used for resolving dependencies.
//
// If an empty string is returned, dependencies should not be resolved.
func (p *Pkg) cleanName() string {
	name := p.Name

	// C 'package' cannot be resolved.
	if name == "C" {
		return ""
	}

	// Internal golang_org/* packages must be prefixed with vendor/
	//
	// Thanks to @davecheney for this:
	// https://github.com/davecheney/graphpkg/blob/master/main.go#L46
	if strings.HasPrefix(name, "golang_org") {
		name = path.Join("vendor", name)
	}

	return name
}

// String returns a string representation of the Pkg containing the Pkg name and status.
func (p *Pkg) String() string {
	if p.Resolved {
		return p.Name
	}
	return p.Name + " (unresolved)"
}

// byInternalAndName ensures a slice of Pkgs are sorted such that the internal stdlib
// packages are always above external packages (ie. github.com/whatever).
type byInternalAndName []Pkg

func (b byInternalAndName) Len() int {
	return len(b)
}

func (b byInternalAndName) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b byInternalAndName) Less(i, j int) bool {
	if b[i].Internal && !b[j].Internal {
		return true
	} else if !b[i].Internal && b[j].Internal {
		return false
	}

	return b[i].Name < b[j].Name
}
