package db

import (
	"context"
	"strings"

	"github.com/uptrace/bun"
)

func MigrateDB(ctx context.Context, dbConn *bun.DB) error {
	models := []any{
		(*APIKey)(nil),
		(*Tag)(nil),
		(*UploadedFile)(nil),
		(*DeletedFileLog)(nil),
		(*FileTag)(nil),
		(*ServiceStats)(nil),
		(*StatusDelta)(nil),
		(*StatusHourly)(nil),
		(*StatusDaily)(nil),
		(*FileStat)(nil),
	}

	for _, model := range models {
		_, err := dbConn.NewCreateTable().
			Model(model).
			IfNotExists().
			WithForeignKeys().
			Exec(ctx)
		if err != nil {
			return err
		}
	}

	_, err := dbConn.NewCreateIndex().
		Model((*UploadedFile)(nil)).
		Index("idx_uploaded_files_file_hash").
		Column("file_hash").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = dbConn.NewCreateIndex().
		Model((*UploadedFile)(nil)).
		Index("idx_uploaded_files_expires_at").
		Column("expires_at").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = dbConn.NewCreateIndex().
		Model((*UploadedFile)(nil)).
		Index("idx_uploaded_files_preview_status").
		Column("preview_status").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = dbConn.NewCreateIndex().
		Model((*DeletedFileLog)(nil)).
		Index("idx_deleted_file_log_purge_at").
		Column("purge_at").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = dbConn.NewCreateIndex().
		Model((*DeletedFileLog)(nil)).
		Index("idx_deleted_file_log_processed").
		Column("processed").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	// Verifica se a coluna existe consultando o PRAGMA do SQLite
	var exists int
	err = dbConn.NewRaw("SELECT count(*) FROM pragma_table_info('uploaded_files') WHERE name = 'preview_webp_path'").
		Scan(ctx, &exists)

	if err != nil {
		return err
	}

	// Se exists == 0, a coluna não foi encontrada, então adicionamos
	if exists == 0 {
		_, err = dbConn.NewRaw("ALTER TABLE uploaded_files ADD COLUMN preview_webp_path TEXT").
			Exec(ctx)
		if err != nil {
			return err
		}
	}

	for _, rollup := range []struct {
		model any
		index string
	}{
		{(*StatusHourly)(nil), "uq_status_hourly_bucket"},
		{(*StatusDaily)(nil), "uq_status_daily_bucket"},
	} {
		_, err := dbConn.NewCreateIndex().
			Model(rollup.model).
			Index(rollup.index).
			Unique().
			Column("ts").
			Column("api_key_id").
			Column("kind").
			Column("content_class").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return err
		}
	}

	_, err = dbConn.NewCreateIndex().
		Model((*StatusDelta)(nil)).
		Index("idx_status_deltas_ts").
		Column("ts").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	for _, col := range []string{
		"received_bytes BIGINT NOT NULL DEFAULT 0",
		"stored_bytes BIGINT NOT NULL DEFAULT 0",
		"files_uploaded BIGINT NOT NULL DEFAULT 0",
		"files_deleted BIGINT NOT NULL DEFAULT 0",
		"bytes_freed BIGINT NOT NULL DEFAULT 0",
		"dedup_hits BIGINT NOT NULL DEFAULT 0",
		"dedup_saved_bytes BIGINT NOT NULL DEFAULT 0",
		"resolver_hits BIGINT NOT NULL DEFAULT 0",
		"auth_failures BIGINT NOT NULL DEFAULT 0",
		"busy_errors BIGINT NOT NULL DEFAULT 0",
		"preview_failures BIGINT NOT NULL DEFAULT 0",
		"backfill_done BOOLEAN NOT NULL DEFAULT 0",
	} {
		if err := ensureColumn(ctx, dbConn, "service_stats", strings.Fields(col)[0], col); err != nil {
			return err
		}
	}

	return nil
}

func ensureColumn(ctx context.Context, dbConn *bun.DB, table, column, ddl string) error {
	var exists int
	err := dbConn.NewRaw("SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).
		Scan(ctx, &exists)
	if err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err = dbConn.NewRaw("ALTER TABLE " + table + " ADD COLUMN " + ddl).Exec(ctx)
	return err
}
