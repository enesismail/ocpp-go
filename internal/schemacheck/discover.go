package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// findModuleRoot walks upward from start until it finds the directory
// holding go.mod, the same way the go command itself locates a module root.
func findModuleRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve module root from %s: %w", start, err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("resolve module root: no go.mod found above %s", start)
		}
		dir = parent
	}
}

// readModulePath extracts the "module <path>" directive from the go.mod at
// moduleRoot, so a Go file's import path can be reconstructed from its
// location on disk without hard-coding the module name.
func readModulePath(moduleRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module")), nil
		}
	}
	return "", fmt.Errorf("read go.mod: no module directive found")
}

// listGoFiles returns every non-test .go file under root, sorted, so file
// discovery is stable across runs and across platforms regardless of the
// order a directory walk happens to visit entries in.
func listGoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list Go files under %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

// listSchemaFiles returns every .json file across dirs, sorted, so the
// schema corpus is walked in a stable order regardless of directory-entry
// order or how many directories -schemas names.
func listSchemaFiles(dirs []string) ([]string, error) {
	var files []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read schema directory %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// importPathForFile reconstructs the import path of the package file
// belongs to, from its location under moduleRoot and the module's own
// declared path — the same identity a real "import" statement referring to
// that package would use.
func importPathForFile(file, moduleRoot, modulePath string) (string, error) {
	absFile, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(moduleRoot, filepath.Dir(absFile))
	if err != nil {
		return "", fmt.Errorf("compute import path for %s: %w", file, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return modulePath, nil
	}
	return modulePath + "/" + rel, nil
}

// treeImportPath computes treeRoot's own import path (e.g. "ocpp2.0.1" on
// disk becomes "github.com/enesismail/ocpp-go/ocpp2.0.1"), the same
// module-relative computation importPathForFile applies to an individual
// file — needed wherever a package path has to be built from the tree root
// without a specific file at hand (the shared types package, "<tree
// import path>/types"). Naively concatenating modulePath and treeRoot's own
// literal text is only correct when treeRoot happens to already be given
// relative to the module root (true for the real "-tree ocpp2.0.1" run
// invoked from the repository root, but not in general — a test fixture
// under testdata/, or a -tree value written any other way, would silently
// mis-key every lookup built on that guess).
func treeImportPath(treeRoot, moduleRoot, modulePath string) (string, error) {
	absTree, err := filepath.Abs(treeRoot)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(moduleRoot, absTree)
	if err != nil {
		return "", fmt.Errorf("compute import path for tree root %s: %w", treeRoot, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return modulePath, nil
	}
	return modulePath + "/" + rel, nil
}

// packageLabelsForFile derives the two package-identifying labels a
// message row carries: profile, the functional-block directory name (e.g.
// "provisioning"), and goPackage, the tree-relative import path (e.g.
// "ocpp2.0.1/provisioning"). A file directly under the tree root (no
// sub-directory) reports an empty profile.
func packageLabelsForFile(file, treeRoot string) (profile, goPackage string, err error) {
	absFile, err := filepath.Abs(file)
	if err != nil {
		return "", "", err
	}
	absRoot, err := filepath.Abs(treeRoot)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(absRoot, filepath.Dir(absFile))
	if err != nil {
		return "", "", err
	}
	rel = filepath.ToSlash(rel)
	treeBase := filepath.Base(filepath.Clean(treeRoot))
	if rel == "." {
		return "", treeBase, nil
	}
	segments := strings.Split(rel, "/")
	return segments[len(segments)-1], treeBase + "/" + rel, nil
}

// hasAllMethods reports whether methods contains every name in want,
// regardless of what else it contains or what order it is in.
func hasAllMethods(methods []string, want ...string) bool {
	set := make(map[string]bool, len(methods))
	for _, m := range methods {
		set[m] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// featureInfo describes one discovered OCPP message: the Feature type that
// carries its GetFeatureName/GetRequestType/GetResponseType method set, the
// request/response Go types it names, and where it lives in the tree.
type featureInfo struct {
	typeName     string
	featureName  string
	requestType  string
	responseType string
	file         string
	profile      string
	goPackage    string
	direction    string
}

// discoverAllFeatures scans every file in goFiles for the
// GetFeatureName/GetRequestType/GetResponseType method-set rule (the only
// reliable message-discovery signal — see the package doc comment for the
// naming counter-examples this rule exists to survive) and returns one
// featureInfo per complete method set found, sorted by feature name for a
// deterministic report.
func discoverAllFeatures(goFiles []string, treeRoot string) ([]featureInfo, error) {
	var features []featureInfo
	for _, file := range goFiles {
		methodSets, err := discoverMethodSets(file)
		if err != nil {
			return nil, err
		}
		for _, ms := range methodSets {
			if !hasAllMethods(ms.Methods, "GetFeatureName", "GetRequestType", "GetResponseType") {
				continue
			}
			profile, goPackage, err := packageLabelsForFile(file, treeRoot)
			if err != nil {
				return nil, err
			}
			features = append(features, featureInfo{
				typeName:     ms.TypeName,
				featureName:  ms.FeatureName,
				requestType:  ms.RequestType,
				responseType: ms.ResponseType,
				file:         file,
				profile:      profile,
				goPackage:    goPackage,
			})
		}
	}
	sort.Slice(features, func(i, j int) bool { return features[i].featureName < features[j].featureName })
	return features, nil
}

// profileRegistration is one ocpp.NewProfile(...) call site: the profile
// name it registers and the bare Feature type names passed as its
// remaining arguments.
type profileRegistration struct {
	name         string
	featureTypes []string
	file         string
}

// discoverProfileRegistrations scans goFiles for every ocpp.NewProfile(...)
// call, resolving the profile-name argument through the file's own
// const/var declarations exactly like discoverMethodSets resolves
// GetFeatureName's return value.
func discoverProfileRegistrations(goFiles []string) ([]profileRegistration, error) {
	var regs []profileRegistration
	for _, file := range goFiles {
		astFile, err := parseASTFile(file)
		if err != nil {
			return nil, err
		}
		consts := fileConstStrings(astFile)
		ast.Inspect(astFile, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewProfile" || len(call.Args) < 1 {
				return true
			}
			name := ""
			switch v := call.Args[0].(type) {
			case *ast.Ident:
				name = consts[v.Name]
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if s, err := strconv.Unquote(v.Value); err == nil {
						name = s
					}
				}
			}
			var featureTypes []string
			for _, arg := range call.Args[1:] {
				composite, ok := arg.(*ast.CompositeLit)
				if !ok {
					continue
				}
				if ident, ok := composite.Type.(*ast.Ident); ok {
					featureTypes = append(featureTypes, ident.Name)
				}
			}
			regs = append(regs, profileRegistration{name: name, featureTypes: featureTypes, file: file})
			return true
		})
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].file < regs[j].file })
	return regs, nil
}

// crossCheckFeaturesAndProfiles hard-fails on any mismatch, in either
// direction, between the Feature types discovered by method set and the
// Feature types registered by an ocpp.NewProfile call: a discovered type a
// profile never registers, or a registered type whose method set is
// incomplete. Matching is scoped by package, since a profile's
// registration file and a Feature type's own declaration file are often
// different files of the same package (e.g. provisioning.go registers
// BootNotificationFeature{}, declared in boot_notification.go).
func crossCheckFeaturesAndProfiles(features []featureInfo, regs []profileRegistration, resolver *TypeResolver) error {
	registeredByPkg := map[string]map[string]bool{}
	for _, reg := range regs {
		pkg, err := resolver.PackageOf(reg.file)
		if err != nil {
			return err
		}
		if registeredByPkg[pkg] == nil {
			registeredByPkg[pkg] = map[string]bool{}
		}
		for _, ft := range reg.featureTypes {
			registeredByPkg[pkg][ft] = true
		}
	}

	discoveredByPkg := map[string]map[string]bool{}
	for _, f := range features {
		pkg, err := resolver.PackageOf(f.file)
		if err != nil {
			return err
		}
		if discoveredByPkg[pkg] == nil {
			discoveredByPkg[pkg] = map[string]bool{}
		}
		discoveredByPkg[pkg][f.typeName] = true
	}

	var problems []string
	for pkg, discovered := range discoveredByPkg {
		registered := registeredByPkg[pkg]
		for name := range discovered {
			if registered == nil || !registered[name] {
				problems = append(problems, fmt.Sprintf("%s.%s implements the full Feature method set but is not registered by any ocpp.NewProfile call", pkg, name))
			}
		}
	}
	for pkg, registered := range registeredByPkg {
		discovered := discoveredByPkg[pkg]
		for name := range registered {
			if discovered == nil || !discovered[name] {
				problems = append(problems, fmt.Sprintf("%s.%s is registered by an ocpp.NewProfile call but does not implement the full Feature method set", pkg, name))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("message discovery does not match profile registrations:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// handlerInterfaceDirections names every handler-interface pair this package
// knows how to read a message direction off, across both message trees this
// tool is pointed at: 2.0.1's CSMSHandler/ChargingStationHandler and 1.6's
// differently-named CentralSystemHandler/ChargePointHandler pair (1.6 calls
// the same two endpoints "Central System" and "Charge Point" rather than
// "CSMS" and "Charging Station" — the same distinction, spelled the way each
// version's own Go tree spells it, so the reported direction stays
// unambiguous rather than borrowing one version's vocabulary for the other's
// interfaces). The two pairs share no interface name, so a single lookup
// table serves either tree with no per-tree branching.
var handlerInterfaceDirections = map[string]string{
	"CSMSHandler":            "CS->CSMS",
	"ChargingStationHandler": "CSMS->CS",
	"CentralSystemHandler":   "CP->CS",
	"ChargePointHandler":     "CS->CP",
}

// discoverDirections scans goFiles for the handler-interface pair every
// functional-block package declares (see handlerInterfaceDirections) and
// reports which side handles each request type: a method declared on the
// interface implemented by the message *recipient* reports the direction
// that recipient receives from. A request type declared on both of a pair's
// interfaces (the real case: DataTransfer, which either endpoint may send to
// the other, in both trees) reports "bidirectional". The result is keyed by
// the request type's own resolved, package-qualified identity, the same
// keying every other tree-wide registry in this package uses.
func discoverDirections(goFiles []string, resolver *TypeResolver) (map[string]string, error) {
	direction := map[string]string{}
	conflicting := map[string]bool{}
	for _, file := range goFiles {
		astFile, err := parseASTFile(file)
		if err != nil {
			return nil, err
		}
		pkg, err := resolver.PackageOf(file)
		if err != nil {
			return nil, err
		}
		for _, decl := range astFile.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				iface, ok := typeSpec.Type.(*ast.InterfaceType)
				if !ok || iface.Methods == nil {
					continue
				}
				dir, ok := handlerInterfaceDirections[typeSpec.Name.Name]
				if !ok {
					continue
				}
				for _, m := range iface.Methods.List {
					fn, ok := m.Type.(*ast.FuncType)
					if !ok || fn.Params == nil {
						continue
					}
					for _, p := range fn.Params.List {
						star, ok := p.Type.(*ast.StarExpr)
						if !ok {
							continue
						}
						ident, ok := star.X.(*ast.Ident)
						if !ok || !strings.HasSuffix(ident.Name, "Request") {
							continue
						}
						key := pkg + "." + ident.Name
						if existing, ok := direction[key]; ok && existing != dir {
							conflicting[key] = true
						} else {
							direction[key] = dir
						}
					}
				}
			}
		}
	}
	for key := range conflicting {
		direction[key] = "bidirectional"
	}
	return direction, nil
}

// structValidatorFunctions scans file for every
// *.RegisterStructValidation(fn, Type1{}, Type2{}, ...) call, returning a
// map from each target type's bare name to the validator function's own
// name — the one fact discoverStructValidators (ast_walk.go) deliberately
// discards, needed here to name which rule fired in a STRUCT_VALIDATOR row.
func structValidatorFunctions(file *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RegisterStructValidation" || len(call.Args) < 2 {
			return true
		}
		fnIdent, ok := call.Args[0].(*ast.Ident)
		if !ok {
			return true
		}
		for _, arg := range call.Args[1:] {
			composite, ok := arg.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if ident, ok := composite.Type.(*ast.Ident); ok {
				out[ident.Name] = fnIdent.Name
			}
		}
		return true
	})
	return out
}
