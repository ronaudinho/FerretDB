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

package pg

import (
	"context"

	"github.com/jackc/pgx/v4"

	"github.com/FerretDB/FerretDB/internal/handlers/common"
	"github.com/FerretDB/FerretDB/internal/handlers/pg/pgdb"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
	"github.com/FerretDB/FerretDB/internal/wire"
)

// MsgListDatabases implements HandlerInterface.
func (h *Handler) MsgListDatabases(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := msg.Document()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var filter *types.Document
	if filter, err = common.GetOptionalParam(document, "filter", filter); err != nil {
		return nil, err
	}

	common.Ignored(document, h.l, "comment", "authorizedDatabases")

	nameOnly, err := common.GetBoolOptionalParam(document, "nameOnly")
	if err != nil {
		return nil, err
	}

	var databases *types.Array
	err = h.pgPool.InTransaction(ctx, func(tx pgx.Tx) error {
		var databaseNames []string
		var err error
		databaseNames, err = pgdb.Databases(ctx, tx)
		if err != nil {
			return lazyerrors.Error(err)
		}

		databases = types.MakeArray(len(databaseNames))
		for _, databaseName := range databaseNames {
			tables, err := pgdb.Tables(ctx, tx, databaseName)
			if err != nil {
				return lazyerrors.Error(err)
			}

			// iterate over result to collect sizes
			var sizeOnDisk int64
			for _, name := range tables {
				var tableSize int64
				fullName := pgx.Identifier{databaseName, name}.Sanitize()
				// If the table was deleted after we got the list of tables, pg_total_relation_size will return null.
				// We use COALESCE to scan this null value as 0 in this case.
				// Even though we run the query in a transaction, the current isolation level doesn't guarantee
				// that the table is not deleted (see https://www.postgresql.org/docs/14/transaction-iso.html).
				err = tx.QueryRow(ctx, "SELECT COALESCE(pg_total_relation_size($1), 0)", fullName).Scan(&tableSize)
				if err != nil {
					return lazyerrors.Error(err)
				}
				sizeOnDisk += tableSize
			}

			d := must.NotFail(types.NewDocument(
				"name", databaseName,
				"sizeOnDisk", sizeOnDisk,
				"empty", sizeOnDisk == 0,
			))

			matches, err := common.FilterDocument(d, filter)
			if err != nil {
				return lazyerrors.Error(err)
			}

			if matches {
				if nameOnly {
					d = must.NotFail(types.NewDocument(
						"name", databaseName,
					))
				}
				if err = databases.Append(d); err != nil {
					return lazyerrors.Error(err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if nameOnly {
		var reply wire.OpMsg
		err = reply.SetSections(wire.OpMsgSection{
			Documents: []*types.Document{must.NotFail(types.NewDocument(
				"databases", databases,
				"ok", float64(1),
			))},
		})
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		return &reply, nil
	}

	var totalSize int64
	err = h.pgPool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&totalSize)
	if err != nil {
		return nil, err
	}

	var reply wire.OpMsg
	err = reply.SetSections(wire.OpMsgSection{
		Documents: []*types.Document{must.NotFail(types.NewDocument(
			"databases", databases,
			"totalSize", totalSize,
			"totalSizeMb", totalSize/1024/1024,
			"ok", float64(1),
		))},
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &reply, nil
}
