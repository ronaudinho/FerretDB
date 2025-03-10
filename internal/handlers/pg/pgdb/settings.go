// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pgdb

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v4"
	"golang.org/x/exp/slices"

	"github.com/FerretDB/FerretDB/internal/fjson"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
)

const (
	// Reserved prefix for database and collection names.
	reservedPrefix = "_ferretdb_"

	// Settings table name.
	settingsTableName = reservedPrefix + "settings"

	// PostgreSQL max table name length.
	maxTableNameLength = 63
)

// createSettingsTable creates FerretDB settings table if it doesn't exist.
// Settings table is used to store FerretDB settings like collections names mapping.
// That table consists of a single document with settings.
func createSettingsTable(ctx context.Context, tx pgx.Tx, db string) error {
	tables, err := tables(ctx, tx, db)
	if err != nil {
		return lazyerrors.Error(err)
	}

	if slices.Contains(tables, settingsTableName) {
		return ErrAlreadyExist
	}

	// TODO use common code for tables/collections: use _jsonb, do not use explicit `CREATE TABLE` SQL there, etc.
	sql := fmt.Sprintf(`CREATE TABLE %s (settings jsonb)`, pgx.Identifier{db, settingsTableName}.Sanitize())
	_, err = tx.Exec(ctx, sql)
	if err != nil {
		pgErr, ok := err.(*pgconn.PgError)
		if !ok {
			return lazyerrors.Errorf("pgdb.createSettingsTable: %w", err)
		}

		switch pgErr.Code {
		case pgerrcode.InvalidSchemaName:
			return ErrTableNotExist
		case pgerrcode.DuplicateTable:
			return ErrAlreadyExist
		case pgerrcode.UniqueViolation, pgerrcode.DuplicateObject:
			// https://www.postgresql.org/message-id/CA+TgmoZAdYVtwBfp1FL2sMZbiHCWT4UPrzRLNnX1Nb30Ku3-gg@mail.gmail.com
			// Reproducible by integration tests.
			return ErrAlreadyExist
		default:
			return lazyerrors.Errorf("pgdb.createSettingsTable: %w", err)
		}
	}

	settings := must.NotFail(types.NewDocument("collections", must.NotFail(types.NewDocument())))
	sql = fmt.Sprintf(`INSERT INTO %s (settings) VALUES ($1)`, pgx.Identifier{db, settingsTableName}.Sanitize())
	_, err = tx.Exec(ctx, sql, must.NotFail(fjson.Marshal(settings)))
	if err != nil {
		return lazyerrors.Error(err)
	}

	return nil
}

// getTableName returns the name of the table for given collection or error.
// If the settings table doesn't exist, it will be created.
// If the record for collection doesn't exist, it will be created.
func getTableName(ctx context.Context, tx pgx.Tx, db, collection string) (string, error) {
	schemaExists, err := schemaExists(ctx, tx, db)
	if err != nil {
		return "", lazyerrors.Error(err)
	}

	if !schemaExists {
		return formatCollectionName(collection), nil
	}

	tables, err := tables(ctx, tx, db)
	if err != nil {
		return "", lazyerrors.Error(err)
	}

	if !slices.Contains(tables, settingsTableName) {
		err = createSettingsTable(ctx, tx, db)
		if err != nil {
			return "", err
		}
	}

	settings, err := getSettingsTable(ctx, tx, db)
	if err != nil {
		return "", lazyerrors.Error(err)
	}

	collectionsDoc := must.NotFail(settings.Get("collections"))
	collections, ok := collectionsDoc.(*types.Document)
	if !ok {
		return "", lazyerrors.Errorf("expected document but got %[1]T: %[1]v", collectionsDoc)
	}

	if collections.Has(collection) {
		return must.NotFail(collections.Get(collection)).(string), nil
	}

	tableName := formatCollectionName(collection)
	must.NoError(collections.Set(collection, tableName))
	must.NoError(settings.Set("collections", collections))

	err = updateSettingsTable(ctx, tx, db, settings)
	if err != nil {
		return "", lazyerrors.Error(err)
	}

	return tableName, nil
}

// getSettingsTable returns FerretDB settings table.
func getSettingsTable(ctx context.Context, tx pgx.Tx, db string) (*types.Document, error) {
	sql := fmt.Sprintf(`SELECT settings FROM %s`, pgx.Identifier{db, settingsTableName}.Sanitize())
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, lazyerrors.Errorf("no settings found")
	}

	var b []byte
	if err := rows.Scan(&b); err != nil {
		return nil, lazyerrors.Error(err)
	}

	doc, err := fjson.Unmarshal(b)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	settings, ok := doc.(*types.Document)
	if !ok {
		return nil, lazyerrors.Errorf("invalid settings document: %v", doc)
	}

	return settings, nil
}

// updateSettingsTable updates FerretDB settings table.
func updateSettingsTable(ctx context.Context, tx pgx.Tx, db string, settings *types.Document) error {
	sql := fmt.Sprintf(`UPDATE %s SET settings = $1`, pgx.Identifier{db, settingsTableName}.Sanitize())
	_, err := tx.Exec(ctx, sql, must.NotFail(fjson.Marshal(settings)))
	return err
}

// removeTableFromSettings removes collection from FerretDB settings table.
func removeTableFromSettings(ctx context.Context, tx pgx.Tx, db, collection string) error {
	settings, err := getSettingsTable(ctx, tx, db)
	if err != nil {
		return lazyerrors.Error(err)
	}

	collections, ok := must.NotFail(settings.Get("collections")).(*types.Document)
	if !ok {
		return lazyerrors.Errorf("invalid settings document")
	}

	if !collections.Has(collection) {
		return ErrTableNotExist
	}

	collections.Remove(collection)

	must.NoError(settings.Set("collections", collections))

	if err := updateSettingsTable(ctx, tx, db, settings); err != nil {
		return lazyerrors.Error(err)
	}

	return nil
}

// formatCollectionName returns collection name in form <shortened_name>_<name_hash>.
func formatCollectionName(name string) string {
	hash32 := fnv.New32a()
	_ = must.NotFail(hash32.Write([]byte(name)))

	nameSymbolsLeft := maxTableNameLength - hash32.Size()*2 - 1
	truncateTo := len(name)
	if truncateTo > nameSymbolsLeft {
		truncateTo = nameSymbolsLeft
	}

	return name[:truncateTo] + "_" + fmt.Sprintf("%x", hash32.Sum([]byte{}))
}
