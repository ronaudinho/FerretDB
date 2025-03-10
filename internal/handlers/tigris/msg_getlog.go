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

package tigris

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/FerretDB/FerretDB/internal/handlers/common"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/logging"
	"github.com/FerretDB/FerretDB/internal/util/must"
	"github.com/FerretDB/FerretDB/internal/util/version"
	"github.com/FerretDB/FerretDB/internal/wire"
)

// MsgGetLog implements HandlerInterface.
func (h *Handler) MsgGetLog(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := msg.Document()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	command := document.Command()

	getLog, err := document.Get(command)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if _, ok := getLog.(types.NullType); ok {
		return nil, common.NewErrorMsg(
			common.ErrMissingField,
			`BSON field 'getLog.getLog' is missing but a required field`,
		)
	}

	if _, ok := getLog.(string); !ok {
		return nil, common.NewError(
			common.ErrTypeMismatch,
			fmt.Errorf(
				"BSON field 'getLog.getLog' is the wrong type '%s', expected type 'string'",
				common.AliasFromType(getLog),
			),
		)
	}

	var resDoc *types.Document
	switch getLog {
	case "*":
		resDoc = must.NotFail(types.NewDocument(
			"names", must.NotFail(types.NewArray("global", "startupWarnings")),
			"ok", float64(1),
		))

	case "global":
		log, err := logging.RecentEntries.GetArray(zapcore.DebugLevel)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		resDoc = must.NotFail(types.NewDocument(
			"log", log,
			"totalLinesWritten", int64(log.Len()),
			"ok", float64(1),
		))

	case "startupWarnings":
		info, err := h.db.Driver.Info(ctx)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var log types.Array
		for _, line := range []string{
			"Powered by FerretDB " + version.Get().Version + " and Tigris " + info.ServerVersion + ".",
			"Please star us on GitHub: https://github.com/FerretDB/FerretDB and https://github.com/tigrisdata/tigris",
		} {
			b, err := json.Marshal(map[string]any{
				"msg":  line,
				"tags": []string{"startupWarnings"},
				"s":    "I",
				"c":    "STORAGE",
				"id":   42000,
				"ctx":  "initandlisten",
				"t": map[string]string{
					"$date": time.Now().UTC().Format("2006-01-02T15:04:05.999Z07:00"),
				},
			})
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			if err = log.Append(string(b)); err != nil {
				return nil, lazyerrors.Error(err)
			}
		}
		resDoc = must.NotFail(types.NewDocument(
			"log", &log,
			"totalLinesWritten", int64(log.Len()),
			"ok", float64(1),
		))

	default:
		return nil, common.NewError(
			common.ErrOperationFailed,
			fmt.Errorf("no RecentEntries named: %s", getLog),
		)
	}

	var reply wire.OpMsg
	err = reply.SetSections(wire.OpMsgSection{Documents: []*types.Document{resDoc}})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &reply, nil
}
