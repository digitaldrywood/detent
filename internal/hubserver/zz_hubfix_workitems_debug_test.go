package hubserver

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

func hubfixWorkitemsDebug(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg != nil {
		return fmt.Sprintf("%v [pg %s table=%s constraint=%s detail=%s message=%s where=%s]", err, pg.Code, pg.TableName, pg.ConstraintName, pg.Detail, pg.Message, pg.Where)
	}
	return fmt.Sprintf("%v [%T]", err, err)
}
