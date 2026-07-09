package db

import (
	"context"

	"github.com/uptrace/bun"
)

func MigrateDB(ctx context.Context, dbConn *bun.DB) error {
	models := []any{
		(*APIKey)(nil),
		(*Tag)(nil),
		(*UploadedFile)(nil),
		(*DeletedFileLog)(nil),
		(*FileTag)(nil),
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

	return nil
}
