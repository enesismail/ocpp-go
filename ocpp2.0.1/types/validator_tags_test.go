package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This is a live guard over the current 2.0.1 source tree, walked from `root`
// below (ocpp2.0.1/, the directory two levels above this file) down through
// every ".go" file it contains. It may pass when the registrations and tags
// agree; it is not a placeholder for generated code — the walk has no
// exclusion list, so a future generated file (a types_gen.go, or a generated
// message file) lands inside `root` and is covered by construction, without
// this test needing an update. A duplicate registration of the same function
// (`monitoringBase`, registered in both set_monitoring_base.go and
// set_monitoring_level.go) was removed when this guard landed; the guard is
// expected to pass against today's tree and to keep guarding whatever
// replaces it.
//
// Known limitation: the walk covers ocpp2.0.1/ only, while the `Validate`
// instance it registers against (types.Validate, aliased from ocppj.Validate)
// is process-global and shared with the 1.6 package tree, which registers its
// own custom tokens against the same instance. A collision — 1.6 and 2.0.1
// registering the same token name for two different functions, with the
// second registration silently overwriting the first — would not be caught
// by this guard, since it never inspects ocpp1.6/. As of this writing there
// are zero such collisions (checked by diffing the two trees' registered
// token sets): the two versions' tokens either differ outright or follow the
// "...201" suffix convention 2.0.1 uses for tags that would otherwise repeat
// a 1.6 name (e.g. registrationStatus201, chargingProfileStatus201). This
// guard does not enforce that convention; it only reports what it can see
// within ocpp2.0.1/.
//
// This guard also does not flag a *dead* registration — a token registered
// with RegisterValidation but never named in any validate tag in the walked
// tree. Three exist today: remoteStartStopStatus201, signatureMethod and
// encodingMethod (the latter two are marked obsolete-from-2.0.1 in
// types.go and kept only for the wire types that still reference the
// concepts by name, not by validator tag). A registered-but-unused token is
// not a coverage problem this test is designed to catch — the guard checks
// that every *used* tag is registered exactly once, not that every
// registered tag is used.
func TestValidatorTagsAreRegisteredExactlyOnce(t *testing.T) {
	tags, registrations := collectValidatorTokens(t)
	builtins := validatorBuiltins()

	// A non-empty floor on how many registrations the walk actually finds,
	// not just on whether the per-tag checks below report zero problems: a
	// walk that silently stopped covering most of the tree (a moved root, a
	// swallowed filepath.Walk error) would still report "no problems" on
	// whatever little it did see, since there would be nothing left to
	// compare against. Today's tree registers 90 distinct validator tokens
	// under ocpp2.0.1/ (counted 2026-08-08); the floor sits comfortably
	// below that so an ordinary, legitimate removal of a few registrations
	// does not trip it, while a walk that stopped covering most of the tree
	// still would.
	const minRegistrations = 80
	if len(registrations) < minRegistrations {
		t.Fatalf("the source walk found only %d validator registrations under ocpp2.0.1/, want at least %d (today's tree registers 90) — it may have silently stopped covering most of the tree", len(registrations), minRegistrations)
	}

	var problems []string
	for tag, locations := range tags {
		if builtins[tag] {
			continue
		}
		if len(registrations[tag]) == 0 {
			problems = append(problems, "unregistered "+tag+" at "+strings.Join(locations, ", "))
		}
	}
	for tag, locations := range registrations {
		if builtins[tag] {
			problems = append(problems, "built-in validator registered explicitly: "+tag)
		} else if len(locations) != 1 {
			problems = append(problems, "validator "+tag+" registered "+strconv.Itoa(len(locations))+" times at "+strings.Join(locations, ", "))
		}
	}

	sort.Strings(problems)
	if len(problems) != 0 {
		t.Fatalf("validator tag coverage problems:\n- %s", strings.Join(problems, "\n- "))
	}
}

// TestValidatorBuiltinsListIsSorted is a self-check on validatorBuiltins'
// source list: the audit in that function's comment is reviewed as a sorted
// list, so an unsorted edit (an addition dropped at the end instead of in
// place) would be easy to miss by eye. Keeping the list provably sorted
// makes every future diff to it a small, alphabetically-located change.
func TestValidatorBuiltinsListIsSorted(t *testing.T) {
	names := strings.Fields(builtinValidatorNames)
	if !sort.StringsAreSorted(names) {
		t.Fatal("builtinValidatorNames must stay sorted (see validatorBuiltins' doc comment)")
	}
}

func collectValidatorTokens(t *testing.T) (map[string][]string, map[string][]string) {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate validator_tags_test.go")
	}
	root := filepath.Dir(filepath.Dir(sourceFile))
	// moduleRoot is the repository root: ocpp2.0.1/ is a direct child of it,
	// and reporting positions relative to it (rather than relative to `root`,
	// or as a bare filepath.Base) disambiguates the three "types.go" files
	// this walk can reach (ocpp2.0.1/types/types.go,
	// ocpp2.0.1/diagnostics/types.go, ocpp2.0.1/display/types.go) — a bare
	// base name collapses all three into the same unhelpful "types.go:N".
	moduleRoot := filepath.Dir(root)
	fset := token.NewFileSet()
	tags := make(map[string][]string)
	registrations := make(map[string][]string)

	err := filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch current := node.(type) {
			case *ast.Field:
				if current.Tag == nil {
					return true
				}
				raw, unquoteErr := strconv.Unquote(current.Tag.Value)
				if unquoteErr != nil {
					t.Fatalf("%s: invalid struct tag: %v", path, unquoteErr)
				}
				validate := reflect.StructTag(raw).Get("validate")
				for _, tokenText := range strings.Split(validate, ",") {
					tokenText = strings.TrimSpace(tokenText)
					if tokenText == "" {
						continue
					}
					// Split on "|" (or-tag alternatives) before stripping any
					// "=param" suffix. Splitting on "=" first, as an earlier
					// version of this walk did, truncates the token string at
					// the first "=" and silently drops every alternative that
					// follows it — a tag shaped "eqfield=Foo|required" would
					// collect only "eqfield" and never see "required" at all.
					for _, alternative := range strings.Split(tokenText, "|") {
						alternative = strings.SplitN(alternative, "=", 2)[0]
						if alternative != "" {
							tags[alternative] = append(tags[alternative], sourcePosition(fset, moduleRoot, path, current.Pos()))
						}
					}
				}
			case *ast.CallExpr:
				selector, isSelector := current.Fun.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != "RegisterValidation" || len(current.Args) == 0 {
					return true
				}
				literal, isLiteral := current.Args[0].(*ast.BasicLit)
				if !isLiteral || literal.Kind != token.STRING {
					t.Fatalf("%s: RegisterValidation must use a string literal", sourcePosition(fset, moduleRoot, path, current.Pos()))
				}
				name, unquoteErr := strconv.Unquote(literal.Value)
				if unquoteErr != nil {
					t.Fatalf("%s: invalid RegisterValidation string: %v", path, unquoteErr)
				}
				registrations[name] = append(registrations[name], sourcePosition(fset, moduleRoot, path, current.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk 2.0.1 source tree: %v", err)
	}
	return tags, registrations
}

func sourcePosition(fset *token.FileSet, moduleRoot, path string, position token.Pos) string {
	relative, relErr := filepath.Rel(moduleRoot, path)
	if relErr != nil {
		relative = path
	}
	return relative + ":" + strconv.Itoa(fset.Position(position).Line)
}

// builtinValidatorNames is every tag go-playground/validator.v9 v9.31.0 (this
// module's direct dependency, gopkg.in/go-playground/validator.v9 in go.mod)
// resolves without a RegisterValidation call, so that a struct tag naming one
// is never flagged as unregistered, and a RegisterValidation call naming one
// is always flagged as redundant.
//
// Audited 2026-08-08 directly against the vendored module cache copy at
// gopkg.in/go-playground/validator.v9@v9.31.0 (`go env GOMODCACHE` to locate
// it): every key of baked_in.go's `bakedInValidators` map (the library's own
// "built-in validators" table); the structural tokens the parser handles
// before a tag ever reaches that map — "dive", "keys", "endkeys",
// "structonly", "omitempty", "nostructlevel", "required" and "isdefault"
// (validator_instance.go's diveTag/keysTag/endKeysTag/structOnlyTag/
// omitempty/noStructLevelTag/requiredTag/isdefault constants) plus "-", the
// skip-field token (skipValidationTag); and "iscolor", the one default
// compound alias baked_in.go's `bakedInAliases` defines. This replaces a
// previous list that both invented tokens this dependency does not define —
// "required_if", "boolean", "json", "jwt", "lowercase" and 38 others never
// appear in v9.31.0 — and omitted real ones it does, including the
// structural "keys"/"endkeys" pair and most of the format validators
// (email, the uuid family, the hostname family, ipv4/ipv6, e164, ...).
//
// The list stays sorted (TestValidatorBuiltinsListIsSorted checks it) so a
// future addition's diff is a single alphabetically-placed line.
const builtinValidatorNames = `- alpha alphanum alphanumunicode alphaunicode ascii base64 base64url btc_addr btc_addr_bech32 cidr cidrv4 cidrv6 contains containsany containsrune datauri dir dive e164 email endkeys endswith eq eqcsfield eqfield eth_addr excludes excludesall excludesrune fieldcontains fieldexcludes file fqdn gt gtcsfield gte gtecsfield gtefield gtfield hexadecimal hexcolor hostname hostname_rfc1123 hsl hsla html html_encoded ip ip4_addr ip6_addr ip_addr ipv4 ipv6 isbn isbn10 isbn13 iscolor isdefault keys latitude len longitude lt ltcsfield lte ltecsfield ltefield ltfield mac max min multibyte ne necsfield nefield nostructlevel number numeric omitempty oneof printascii required required_with required_with_all required_without required_without_all rgb rgba ssn startswith structonly tcp4_addr tcp6_addr tcp_addr udp4_addr udp6_addr udp_addr unique unix_addr uri url url_encoded urn_rfc2141 uuid uuid3 uuid3_rfc4122 uuid4 uuid4_rfc4122 uuid5 uuid5_rfc4122 uuid_rfc4122`

func validatorBuiltins() map[string]bool {
	result := make(map[string]bool)
	for _, name := range strings.Fields(builtinValidatorNames) {
		result[name] = true
	}
	return result
}
