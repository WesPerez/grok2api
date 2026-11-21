package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func TestDefaultConfigPathFindsRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	backendDir := filepath.Join(root, "backend")
	if err := os.Mkdir(backendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(backendDir)

	if path := defaultConfigPath(); path != filepath.Join("..", "config.yaml") {
		t.Fatalf("defaultConfigPath() = %q", path)
	}
}

func TestSchemaOnlyPreservesOfflineStateWithoutBootstrappingApplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	cfg := config.Config{Database: config.DatabaseConfig{Driver: "sqlite", SQLite: config.SQLiteDatabaseConfig{Path: path}}}
	for range 2 {
		if err := initializeSchemaOnly(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	db, err := relational.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Schema-only must not call app.New and create a bootstrap administrator.
	if count, err := relational.NewAdminRepository(db).Count(context.Background()); err != nil || count != 0 {
		t.Fatalf("schema-only administrator count = %d, %v", count, err)
	}
	options, err := parseOptions([]string{"--schema-only", "--config", "isolated.yaml"})
	if err != nil || !options.schemaOnly || options.configPath != "isolated.yaml" {
		t.Fatalf("schema-only options = %#v, %v", options, err)
	}
	if err := Run([]string{"--schema-only", "--config", filepath.Join(t.TempDir(), "absent.yaml")}); err == nil {
		t.Fatal("missing migration configuration was accepted")
	}
}

func TestParseOptionsSupportsContainerListenOverride(t *testing.T) {
	options, err := parseOptions([]string{"--config", "/app/config.yaml", "--listen", "0.0.0.0:8000"})
	if err != nil {
		t.Fatal(err)
	}
	if options.configPath != "/app/config.yaml" || options.listen != "0.0.0.0:8000" {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseOptions([]string{"--listen"}); err == nil {
		t.Fatal("missing --listen value was accepted")
	}
}
