package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/chenyme/grok2api/backend/internal/app"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// Run 解析启动参数并运行后端服务。
func Run(args []string) error {
	options, err := parseOptions(args)
	if err != nil {
		return err
	}
	if options.schemaOnly {
		if info, err := os.Stat(options.configPath); err != nil || !info.Mode().IsRegular() {
			return errors.New("schema-only requires an existing configuration file")
		}
	}
	cfg, err := config.Load(options.configPath)
	if err != nil {
		return err
	}
	if options.listen != "" {
		cfg.Server.Listen = options.listen
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	if options.schemaOnly {
		return initializeSchemaOnly(context.Background(), cfg)
	}
	logger := observability.NewLogger()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer application.Close()
	return application.Run(ctx)
}

type runOptions struct {
	configPath string
	listen     string
	schemaOnly bool
}

// Schema validation must never construct providers or start background workers.
func initializeSchemaOnly(ctx context.Context, cfg config.Config) error {
	var database *relational.Database
	var err error
	switch cfg.Database.Driver {
	case "sqlite":
		database, err = relational.OpenSQLite(ctx, cfg.Database.SQLite.Path)
	case "postgres":
		database, err = relational.OpenPostgres(ctx, cfg.Database.Postgres.DSN, cfg.Database.Postgres.MaxOpenConns, cfg.Database.Postgres.MaxIdleConns)
	default:
		return fmt.Errorf("unsupported schema database driver: %s", cfg.Database.Driver)
	}
	if err != nil {
		return err
	}
	return errors.Join(database.InitializeSchema(ctx), database.Close())
}

func parseOptions(args []string) (runOptions, error) {
	options := runOptions{configPath: defaultConfigPath()}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--schema-only":
			options.schemaOnly = true
		case "--config":
			if index+1 >= len(args) {
				return runOptions{}, errors.New("--config 缺少路径")
			}
			options.configPath = args[index+1]
			index++
		case "--listen":
			if index+1 >= len(args) {
				return runOptions{}, errors.New("--listen 缺少地址")
			}
			options.listen = args[index+1]
			index++
		default:
			return runOptions{}, fmt.Errorf("不支持的启动参数: %s", args[index])
		}
	}
	return options, nil
}

func defaultConfigPath() string {
	for _, candidate := range []string{"config.yaml", filepath.Join("..", "config.yaml")} {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	return "config.yaml"
}
