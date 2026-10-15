package model

import (
	"context"
	"errors"

	jetgen "github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/table"
	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
)

// enabledConfiguration reads the one enabled configuration row. The read is a
// single statement, so a concurrent change either lands before it and is
// reflected whole or after it and leaves this snapshot consistent. A missing
// row and a disabled one are the same outcome: there is no enabled
// configuration to issue from.
func enabledConfiguration(ctx context.Context, db qrm.Queryable) (Configuration, error) {
	var row jetgen.ModelConfigurations
	err := table.ModelConfigurations.
		SELECT(table.ModelConfigurations.WorkspaceID, table.ModelConfigurations.Model).
		FROM(table.ModelConfigurations).
		WHERE(table.ModelConfigurations.Enabled.EQ(postgres.Bool(true))).
		QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return Configuration{}, ErrConfigurationUnavailable
	}
	if err != nil {
		return Configuration{}, err
	}
	return Configuration{WorkspaceID: row.WorkspaceID, Model: row.Model}, nil
}
