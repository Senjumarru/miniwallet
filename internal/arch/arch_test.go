package arch_test

import (
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/Senjumarru/miniwallet/"

// пакет -> что ему импортировать запрещено
var forbidden = map[string][]string{
	"internal/domain":         {"internal/"},
	"internal/service":        {"internal/storage", "internal/httpapi"},
	"internal/storage/sqlite": {"internal/service", "internal/httpapi", "internal/provider"},
	"internal/storage/memory": {"internal/service", "internal/httpapi", "internal/provider"},
	"internal/provider":       {"internal/service", "internal/httpapi", "internal/storage"},
}

func TestLayering(t *testing.T) {
	out, err := exec.Command("go", "list", "-f",
		"{{.ImportPath}} {{join .Imports \" \"}}", mod+"internal/...").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		pkg := strings.TrimPrefix(fields[0], mod)
		for _, imp := range fields[1:] {
			if !strings.HasPrefix(imp, mod) {
				continue
			}
			rel := strings.TrimPrefix(imp, mod)
			for _, bad := range forbidden[pkg] {
				if strings.HasPrefix(rel, bad) {
					t.Errorf("%s must not import %s", pkg, rel)
				}
			}
		}
	}
}

func TestServiceDoesNotImportDatabaseSQL(t *testing.T) {
	out, err := exec.Command("go", "list", "-f",
		"{{.ImportPath}} {{join .Imports \" \"}}", mod+"internal/service").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(out))
	for _, imp := range fields[1:] {
		if imp == "database/sql" {
			t.Errorf("internal/service must not import database/sql (imported in interfaces.go, payment.go, reconciler.go)")
		}
	}
}
