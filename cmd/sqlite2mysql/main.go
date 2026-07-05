package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/model"

	sqlite "github.com/glebarez/sqlite"
	drivermysql "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var migrationModels = []any{
	&model.Channel{},
	&model.Token{},
	&model.User{},
	&model.PasskeyCredential{},
	&model.Option{},
	&model.Redemption{},
	&model.Ability{},
	&model.Log{},
	&model.Midjourney{},
	&model.TopUp{},
	&model.QuotaData{},
	&model.Task{},
	&model.Model{},
	&model.Vendor{},
	&model.PrefillGroup{},
	&model.Setup{},
	&model.TwoFA{},
	&model.TwoFABackupCode{},
	&model.Checkin{},
	&model.SubscriptionOrder{},
	&model.UserSubscription{},
	&model.SubscriptionPreConsumeRecord{},
	&model.CustomOAuthProvider{},
	&model.UserOAuthBinding{},
	&model.PerfMetric{},
	&model.SubscriptionPlan{},
}

type tableSummary struct {
	Name  string
	Rows  int64
	Error error
}

func main() {
	sqlitePath := flag.String("sqlite", "one-api.db", "path to the source SQLite database")
	mysqlDSN := flag.String("dsn", "", "target MySQL DSN, e.g. user:pass@tcp(127.0.0.1:3306)/new_api_test?charset=utf8mb4&parseTime=true&loc=Local")
	reset := flag.Bool("reset", false, "drop known project tables in the target MySQL database before migrating")
	createDB := flag.Bool("create-db", false, "create the target MySQL database if it does not exist")
	charset := flag.String("charset", "utf8mb4", "character set to use when -create-db creates the target database")
	collation := flag.String("collation", "utf8mb4_unicode_ci", "collation to use when -create-db creates the target database")
	dryRun := flag.Bool("dry-run", false, "only inspect the SQLite database and print table counts")
	batchSize := flag.Int("batch-size", 500, "rows per insert batch")
	flag.Parse()

	if *mysqlDSN == "" && !*dryRun {
		fatalf("-dsn is required unless -dry-run is set")
	}
	if *batchSize <= 0 {
		fatalf("-batch-size must be greater than 0")
	}
	if _, err := os.Stat(*sqlitePath); err != nil {
		fatalf("source SQLite database is not accessible: %v", err)
	}

	source, err := gorm.Open(sqlite.Open(*sqlitePath), &gorm.Config{})
	if err != nil {
		fatalf("open SQLite database: %v", err)
	}

	tables, err := listSQLiteTables(source)
	if err != nil {
		fatalf("list SQLite tables: %v", err)
	}
	if len(tables) == 0 {
		fatalf("no user tables found in SQLite database")
	}

	summaries := summarizeTables(source, tables)
	printSummaries("source SQLite", summaries)
	if *dryRun {
		return
	}

	targetDSN := ensureParseTime(*mysqlDSN)
	if *createDB {
		if err := createMySQLDatabase(targetDSN, *charset, *collation); err != nil {
			fatalf("create MySQL database: %v", err)
		}
	}

	target, err := gorm.Open(gormmysql.Open(targetDSN), &gorm.Config{})
	if err != nil {
		fatalf("open MySQL database: %v", err)
	}

	if *reset {
		log.Printf("reset requested: dropping known project tables from target MySQL database")
		if err := dropKnownTables(target); err != nil {
			fatalf("drop target tables: %v", err)
		}
	}

	log.Printf("creating/updating MySQL schema with project GORM models")
	if err := target.AutoMigrate(migrationModels...); err != nil {
		fatalf("auto migrate MySQL schema: %v", err)
	}

	if err := ensureTargetEmpty(target, tables); err != nil {
		fatalf("%v", err)
	}

	if err := target.Exec("SET FOREIGN_KEY_CHECKS = 0").Error; err != nil {
		fatalf("disable MySQL foreign key checks: %v", err)
	}
	defer func() {
		if err := target.Exec("SET FOREIGN_KEY_CHECKS = 1").Error; err != nil {
			log.Printf("warning: failed to re-enable MySQL foreign key checks: %v", err)
		}
	}()

	for _, table := range tables {
		copied, err := copyTable(source, target, table, *batchSize)
		if err != nil {
			fatalf("copy table %s: %v", table, err)
		}
		if err := syncAutoIncrement(source, target, table); err != nil {
			fatalf("sync auto increment for table %s: %v", table, err)
		}
		log.Printf("copied %-40s %d rows", table, copied)
	}

	targetSummaries := summarizeTables(target, tables)
	printSummaries("target MySQL", targetSummaries)
	if err := compareSummaries(summaries, targetSummaries); err != nil {
		fatalf("verification failed: %v", err)
	}

	log.Printf("migration completed successfully")
}

func ensureParseTime(dsn string) string {
	if strings.Contains(dsn, "parseTime=") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&parseTime=true"
	}
	return dsn + "?parseTime=true"
}

func createMySQLDatabase(dsn string, charset string, collation string) error {
	cfg, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		return err
	}
	dbName := cfg.DBName
	if dbName == "" {
		return fmt.Errorf("DSN does not include a database name")
	}
	cfg.DBName = ""

	adminDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer adminDB.Close()

	createSQL := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS %s CHARACTER SET %s COLLATE %s",
		quoteMySQLIdentifier(dbName),
		quoteMySQLIdentifierPart(charset),
		quoteMySQLIdentifierPart(collation),
	)
	if _, err := adminDB.Exec(createSQL); err != nil {
		return err
	}
	log.Printf("ensured MySQL database %q exists", dbName)
	return nil
}

func listSQLiteTables(db *gorm.DB) ([]string, error) {
	rows, err := db.Raw(`
		SELECT name
		FROM sqlite_master
		WHERE type = 'table'
			AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

func summarizeTables(db *gorm.DB, tables []string) []tableSummary {
	summaries := make([]tableSummary, 0, len(tables))
	for _, table := range tables {
		var count int64
		err := db.Table(table).Count(&count).Error
		summaries = append(summaries, tableSummary{
			Name:  table,
			Rows:  count,
			Error: err,
		})
	}
	return summaries
}

func printSummaries(label string, summaries []tableSummary) {
	log.Printf("%s table counts:", label)
	for _, summary := range summaries {
		if summary.Error != nil {
			log.Printf("  %-40s error: %v", summary.Name, summary.Error)
			continue
		}
		log.Printf("  %-40s %d", summary.Name, summary.Rows)
	}
}

func dropKnownTables(db *gorm.DB) error {
	for i := len(migrationModels) - 1; i >= 0; i-- {
		if err := db.Migrator().DropTable(migrationModels[i]); err != nil {
			return err
		}
	}
	return nil
}

func ensureTargetEmpty(db *gorm.DB, tables []string) error {
	var nonEmpty []string
	for _, table := range tables {
		if !db.Migrator().HasTable(table) {
			return fmt.Errorf("target MySQL table %q does not exist after migration", table)
		}
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil {
			return fmt.Errorf("count target table %q: %w", table, err)
		}
		if count > 0 {
			nonEmpty = append(nonEmpty, fmt.Sprintf("%s=%d", table, count))
		}
	}
	if len(nonEmpty) > 0 {
		return fmt.Errorf("target MySQL database is not empty (%s); use a fresh test database or pass -reset", strings.Join(nonEmpty, ", "))
	}
	return nil
}

func copyTable(source *gorm.DB, target *gorm.DB, table string, batchSize int) (int64, error) {
	rows, err := source.Table(table).Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if len(columns) == 0 {
		return 0, nil
	}

	var copied int64
	batch := make([]map[string]any, 0, batchSize)
	for rows.Next() {
		record, err := scanRecord(rows, columns)
		if err != nil {
			return copied, err
		}
		batch = append(batch, record)
		if len(batch) >= batchSize {
			if err := target.Table(table).Create(batch).Error; err != nil {
				return copied, err
			}
			copied += int64(len(batch))
			batch = batch[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return copied, err
	}
	if len(batch) > 0 {
		if err := target.Table(table).Create(batch).Error; err != nil {
			return copied, err
		}
		copied += int64(len(batch))
	}
	return copied, nil
}

func scanRecord(rows *sql.Rows, columns []string) (map[string]any, error) {
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}

	record := make(map[string]any, len(columns))
	for i, column := range columns {
		record[column] = normalizeSQLValue(values[i])
	}
	return record, nil
}

func normalizeSQLValue(value any) any {
	switch v := value.(type) {
	case []byte:
		return string(v)
	default:
		return v
	}
}

func syncAutoIncrement(source *gorm.DB, target *gorm.DB, table string) error {
	if !tableHasColumn(source, table, "id") {
		return nil
	}

	var maxID sql.NullInt64
	if err := source.Table(table).Select("MAX(id)").Scan(&maxID).Error; err != nil {
		return err
	}
	if !maxID.Valid || maxID.Int64 < 1 {
		return nil
	}

	sql := fmt.Sprintf(
		"ALTER TABLE %s AUTO_INCREMENT = %d",
		quoteMySQLIdentifier(table),
		maxID.Int64+1,
	)
	return target.Exec(sql).Error
}

func tableHasColumn(db *gorm.DB, table string, column string) bool {
	rows, err := db.Raw("PRAGMA table_info(" + quoteSQLiteIdentifier(table) + ")").Rows()
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

func compareSummaries(source []tableSummary, target []tableSummary) error {
	targetByName := make(map[string]tableSummary, len(target))
	for _, summary := range target {
		targetByName[summary.Name] = summary
	}
	for _, sourceSummary := range source {
		if sourceSummary.Error != nil {
			return fmt.Errorf("source table %s count failed: %w", sourceSummary.Name, sourceSummary.Error)
		}
		targetSummary, ok := targetByName[sourceSummary.Name]
		if !ok {
			return fmt.Errorf("target table %s is missing", sourceSummary.Name)
		}
		if targetSummary.Error != nil {
			return fmt.Errorf("target table %s count failed: %w", targetSummary.Name, targetSummary.Error)
		}
		if sourceSummary.Rows != targetSummary.Rows {
			return fmt.Errorf("row count mismatch for %s: source=%d target=%d", sourceSummary.Name, sourceSummary.Rows, targetSummary.Rows)
		}
	}
	return nil
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func quoteMySQLIdentifier(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}

func quoteMySQLIdentifierPart(identifier string) string {
	if identifier == "" {
		return ""
	}
	for _, r := range identifier {
		if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			fatalf("unsafe MySQL identifier part %q", identifier)
		}
	}
	return identifier
}

func fatalf(format string, args ...any) {
	log.Fatalf(format, args...)
}
