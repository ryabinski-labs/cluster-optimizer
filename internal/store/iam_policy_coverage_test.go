package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWriterIAMPolicyCoversAllActions is a guardrail for the kind of bug that
// broke production on 2026-05-23: a refactor inside *DynamoDBWriter started
// calling a new DynamoDB API (BatchWriteItem) but the CloudFormation-managed
// IAM policy at infra/cloudformation/dynamodb-writer-policy.yaml didn't list
// the new action, so the CronJob hit AccessDeniedException on every tick.
//
// This test parses the store package's AST, finds every dynamodb-client
// method called from a *DynamoDBWriter receiver, and asserts that every
// resulting dynamodb:<Action> appears in the writer policy. A diff between
// the two sides fails the build before the policy drifts again.
func TestWriterIAMPolicyCoversAllActions(t *testing.T) {
	used, err := dynamoActionsCalledByWriter()
	if err != nil {
		t.Fatalf("scan writer source: %v", err)
	}
	if len(used) == 0 {
		t.Fatal("scanned the store package but found no DynamoDB calls inside *DynamoDBWriter methods — the AST walker is broken")
	}

	allowed, err := writerPolicyAllowedActions()
	if err != nil {
		t.Fatalf("read writer policy: %v", err)
	}

	var missing []string
	for action := range used {
		if !allowed[action] {
			missing = append(missing, action)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("writer policy is missing actions used by *DynamoDBWriter: %v\n"+
			"add these to infra/cloudformation/dynamodb-writer-policy.yaml under WriteClusterOptimizerReports.Action",
			missing)
	}
}

// dynamoActionsCalledByWriter returns the set of dynamodb:<Action> names
// reachable from any method whose receiver is *DynamoDBWriter. The walker
// follows static calls between writer methods so helpers like batchWrite
// (which actually issues the BatchWriteItem) are attributed to their
// caller's policy surface.
func dynamoActionsCalledByWriter() (map[string]bool, error) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	pkgDir := filepath.Join(repoRoot, "internal", "store")

	fset := token.NewFileSet()
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, err
	}

	// Map: function name (writer methods only) -> body.
	writerFuncs := map[string]*ast.FuncDecl{}
	// Map: non-writer helpers that take a *dynamodb.Client param (so they
	// can be called from writer methods and still consume policy surface).
	helperFuncs := map[string]*ast.FuncDecl{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if isWriterMethod(fn) {
				writerFuncs[fn.Name.Name] = fn
				continue
			}
			helperFuncs[fn.Name.Name] = fn
		}
	}

	actions := map[string]bool{}
	visited := map[string]bool{}

	var walk func(fn *ast.FuncDecl)
	walk = func(fn *ast.FuncDecl) {
		if fn == nil || fn.Body == nil {
			return
		}
		if visited[fn.Name.Name] {
			return
		}
		visited[fn.Name.Name] = true

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				// Pattern 1: client.<Action>(...) — direct dynamodb call.
				if name := dynamoActionFromCall(fun); name != "" {
					actions[name] = true
				}
				// Pattern 2: w.<method>(...) — recurse into peer writer method.
				if ident, ok := fun.X.(*ast.Ident); ok && ident.Name == "w" {
					if peer, ok := writerFuncs[fun.Sel.Name]; ok {
						walk(peer)
					}
				}
			case *ast.Ident:
				// Pattern 3: helperFn(...) — non-method helper in the same package.
				if helper, ok := helperFuncs[fun.Name]; ok {
					walk(helper)
				}
			}
			return true
		})
	}

	for _, fn := range writerFuncs {
		walk(fn)
	}
	return actions, nil
}

// dynamoActionFromCall returns "dynamodb:Action" when the selector expression
// reaches the DynamoDB SDK client, otherwise "". Two shapes count:
//   - client.Action(...)      // top-level reader helpers
//   - <recv>.client.Action(...) // writer methods (w.client.Action)
func dynamoActionFromCall(sel *ast.SelectorExpr) string {
	switch x := sel.X.(type) {
	case *ast.Ident:
		if x.Name == "client" {
			return "dynamodb:" + sel.Sel.Name
		}
	case *ast.SelectorExpr:
		if x.Sel.Name == "client" {
			return "dynamodb:" + sel.Sel.Name
		}
	}
	return ""
}

// isWriterMethod reports whether fn is declared with a *DynamoDBWriter receiver.
func isWriterMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "DynamoDBWriter"
}

// writerPolicyAllowedActions returns the set of dynamodb:* actions granted by
// the WriteClusterOptimizerReports statement in the CloudFormation template.
func writerPolicyAllowedActions() (map[string]bool, error) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(repoRoot, "infra", "cloudformation", "dynamodb-writer-policy.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var tmpl struct {
		Resources struct {
			ClusterOptimizerDynamoDBWriterPolicy struct {
				Properties struct {
					PolicyDocument struct {
						Statement []struct {
							Sid    string   `yaml:"Sid"`
							Effect string   `yaml:"Effect"`
							Action []string `yaml:"Action"`
						} `yaml:"Statement"`
					} `yaml:"PolicyDocument"`
				} `yaml:"Properties"`
			} `yaml:"ClusterOptimizerDynamoDBWriterPolicy"`
		} `yaml:"Resources"`
	}
	if err := yaml.Unmarshal(raw, &tmpl); err != nil {
		return nil, err
	}

	allowed := map[string]bool{}
	for _, stmt := range tmpl.Resources.ClusterOptimizerDynamoDBWriterPolicy.Properties.PolicyDocument.Statement {
		if stmt.Effect != "Allow" {
			continue
		}
		for _, a := range stmt.Action {
			allowed[a] = true
		}
	}
	return allowed, nil
}

// findRepoRoot walks up from the working directory until it finds go.mod.
// Tests run with CWD set to the package dir, so this gives us a stable
// anchor without hardcoding paths.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
