package dbscope_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// The request handlers must reach the database only through dbscope.DB. This
// reads each handler's struct and fails if it holds a pgx pool, or does not
// hold a *dbscope.DB (which would make the check vacuous after a rename).
// Limits: it looks at the handler struct only, not at package-level variables
// or at what a constructor could pass in some other way.
func TestUserHandlersHoldNoPool(t *testing.T) {
	handlers := []struct{ dir, typeName string }{
		{"../groups", "Handler"},
		{"../expenses", "Handler"},
		{"../balances", "Handler"},
		{"../settlements", "Handler"},
		{"../receiptapi", "Handler"},
	}
	for _, h := range handlers {
		t.Run(h.dir, func(t *testing.T) {
			fset := token.NewFileSet()
			pkgs, err := parser.ParseDir(fset, h.dir, func(fi fs.FileInfo) bool { //nolint:staticcheck // ParseDir ignores build tags; these handler packages have no tagged files, and the alternative (go/packages) is a heavy dependency for a source-shape check
				return !strings.HasSuffix(fi.Name(), "_test.go")
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			found, holdsScope := false, false
			for _, pkg := range pkgs {
				for _, f := range pkg.Files {
					ast.Inspect(f, func(n ast.Node) bool {
						ts, ok := n.(*ast.TypeSpec)
						if !ok || ts.Name.Name != h.typeName {
							return true
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							return true
						}
						found = true
						for _, field := range st.Fields.List {
							ast.Inspect(field.Type, func(m ast.Node) bool {
								if sel, ok := m.(*ast.SelectorExpr); ok {
									x, _ := sel.X.(*ast.Ident)
									switch {
									case x != nil && x.Name == "pgxpool":
										t.Errorf("%s.%s has a field of type pgxpool.%s", h.dir, h.typeName, sel.Sel.Name)
									case x != nil && x.Name == "dbscope" && sel.Sel.Name == "DB":
										holdsScope = true
									}
								}
								return true
							})
						}
						return false
					})
				}
			}
			if !found {
				t.Fatalf("type %s not found in %s", h.typeName, h.dir)
			}
			if !holdsScope {
				t.Errorf("%s.%s does not hold a *dbscope.DB", h.dir, h.typeName)
			}
		})
	}
}
