package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestInitializeSchemaAddsConsoleCachesWithoutLosingCredentials(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	now := time.Now().UTC().Truncate(time.Second)

	build, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "legacy-build", SourceKey: "legacy-build-cache-upgrade",
		EncryptedAccessToken: "preserved-build-access", EncryptedRefreshToken: "preserved-build-refresh",
		ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpdateCredentialRefreshFailure(ctx, build.ID, repository.CredentialRefreshFailure{
		Count: 3, UnclassifiedAuthFailureCount: 2, RetryAt: now.Add(10 * time.Minute),
		Status: 401, Code: "oauth_http_401", Message: "Unauthorized", Response: "redacted response",
	}); err != nil {
		t.Fatal(err)
	}
	console, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
		Name: "legacy-console", SourceKey: "legacy-console-cache-upgrade",
		EncryptedAccessToken: "preserved-console-session", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
		Name: "legacy-web", SourceKey: "legacy-web-cache-upgrade",
		EncryptedAccessToken: "preserved-web-session", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.Create(&webConsoleAccountLinkModel{
		WebAccountID: web.ID, ConsoleAccountID: console.ID, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveQuotaWindows(ctx, console.ID, "", now, []account.QuotaWindow{{
		AccountID: console.ID, Mode: "console", Remaining: 7, Total: 9, WindowSeconds: 3600,
		Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		if err := database.db.WithContext(ctx).Migrator().DropConstraint(&accountCredentialModel{}, "chk_account_credentials_console_api_key_id"); err != nil {
			return err
		}
		if err := database.db.WithContext(ctx).Migrator().DropConstraint(&accountCredentialModel{}, "chk_account_credentials_console_workos_session"); err != nil {
			return err
		}
		if err := database.db.WithContext(ctx).Migrator().DropColumn(&accountCredentialModel{}, "ConsoleAPIKeyID"); err != nil {
			return err
		}
		return database.db.WithContext(ctx).Migrator().DropColumn(&accountCredentialModel{}, "EncryptedConsoleWorkOSSession")
	}); err != nil {
		t.Fatal(err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("upgrade legacy account_credentials schema: %v", err)
	}
	for _, column := range []string{"console_api_key_id", "encrypted_console_workos_session"} {
		if !database.db.Migrator().HasColumn(&accountCredentialModel{}, column) {
			t.Fatalf("missing upgraded column %q", column)
		}
	}
	var defaults struct {
		APIKeyNotNull  int    `gorm:"column:api_key_not_null"`
		APIKeyDefault  string `gorm:"column:api_key_default"`
		SessionNotNull int    `gorm:"column:session_not_null"`
		SessionDefault string `gorm:"column:session_default"`
	}
	if err := database.db.Raw(`SELECT
		MAX(CASE WHEN name = 'console_api_key_id' THEN "notnull" ELSE 0 END) AS api_key_not_null,
		MAX(CASE WHEN name = 'console_api_key_id' THEN dflt_value ELSE '' END) AS api_key_default,
		MAX(CASE WHEN name = 'encrypted_console_workos_session' THEN "notnull" ELSE 0 END) AS session_not_null,
		MAX(CASE WHEN name = 'encrypted_console_workos_session' THEN dflt_value ELSE '' END) AS session_default
		FROM pragma_table_info('account_credentials')`).Scan(&defaults).Error; err != nil {
		t.Fatal(err)
	}
	if defaults.APIKeyNotNull != 1 || defaults.SessionNotNull != 1 || !isSQLiteEmptyStringDefault(defaults.APIKeyDefault) || !isSQLiteEmptyStringDefault(defaults.SessionDefault) {
		t.Fatalf("cache column definitions = %#v", defaults)
	}
	for _, column := range []string{"console_api_key_id", "encrypted_console_workos_session"} {
		var value string
		query := "SELECT " + column + " FROM account_credentials WHERE account_id = ?"
		if err := database.db.Raw(query, console.ID).Scan(&value).Error; err != nil {
			t.Fatal(err)
		}
		if value != "" {
			t.Fatalf("%s value after schema upgrade = %q, want empty", column, value)
		}
	}
	for _, oversized := range []struct {
		column string
		value  string
	}{
		{column: "console_api_key_id", value: strings.Repeat("x", 65)},
		{column: "encrypted_console_workos_session", value: strings.Repeat("x", 65537)},
	} {
		if err := database.db.Exec("UPDATE account_credentials SET "+oversized.column+" = ? WHERE account_id = ?", oversized.value, console.ID).Error; err == nil {
			t.Fatalf("oversized %s cache was accepted", oversized.column)
		}
	}

	storedBuild, err := accounts.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedBuild.EncryptedAccessToken != "preserved-build-access" || storedBuild.EncryptedRefreshToken != "preserved-build-refresh" || storedBuild.RefreshFailureCount != 3 || storedBuild.LastRefreshErrorStatus != 401 || storedBuild.LastRefreshErrorCode != "oauth_http_401" || storedBuild.LastRefreshErrorMessage != "Unauthorized" {
		t.Fatalf("Build credential after schema upgrade = %#v", storedBuild)
	}
	material, err := accounts.GetCredentialMaterial(ctx, console.ID, account.ProviderConsole)
	if err != nil {
		t.Fatal(err)
	}
	if material.EncryptedAccessToken != "preserved-console-session" {
		t.Fatalf("Console session after schema upgrade = %#v", material)
	}
	var links []webConsoleAccountLinkModel
	if err := database.db.Where("web_account_id = ? AND console_account_id = ?", web.ID, console.ID).Find(&links).Error; err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("Web-to-Console link after schema upgrade = %#v", links)
	}
	windows, err := accounts.GetQuotaWindows(ctx, []uint64{console.ID})
	if err != nil || len(windows[console.ID]) != 1 || windows[console.ID][0].Remaining != 7 {
		t.Fatalf("Console quota after schema upgrade = %#v, err=%v", windows[console.ID], err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("repeat schema initialization: %v", err)
	}
	var integrity string
	if err := database.db.Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
	var foreignKeyViolation string
	if err := database.db.Raw("PRAGMA foreign_key_check").Scan(&foreignKeyViolation).Error; err != nil {
		t.Fatal(err)
	}
	if foreignKeyViolation != "" {
		t.Fatalf("foreign_key_check found a violation in %q", foreignKeyViolation)
	}
}

func isSQLiteEmptyStringDefault(value string) bool {
	switch strings.TrimSpace(value) {
	case "''", "\"\"":
		return true
	default:
		return false
	}
}
