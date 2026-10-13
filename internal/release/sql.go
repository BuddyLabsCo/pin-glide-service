package release

import (
	"encoding/json"
	"fmt"

	pgquery "github.com/wasilibs/go-pgquery"
)

// validateSQL rejects transaction control (which would break the migration ledger)
// and explicit cluster/database-identity changes. Release SQL is trusted code, not
// a sandbox: procedural bodies and invoked functions must obey the same contract.
func validateSQL(statement string) error {
	parsed, err := pgquery.ParseToJSON(statement)
	if err != nil {
		return fmt.Errorf("parse release SQL: %w", err)
	}
	var tree any
	if err := json.Unmarshal([]byte(parsed), &tree); err != nil {
		return err
	}
	return validateSQLNode(tree)
}

func validateSQLNode(node any) error {
	switch node := node.(type) {
	case []any:
		for _, value := range node {
			if err := validateSQLNode(value); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, value := range node {
			switch key {
			case "TransactionStmt", "CreateRoleStmt", "AlterRoleStmt", "AlterRoleSetStmt", "DropRoleStmt", "GrantRoleStmt",
				"CreatedbStmt", "AlterDatabaseStmt", "AlterDatabaseSetStmt", "AlterDatabaseRefreshCollStmt", "DropdbStmt",
				"CreateTableSpaceStmt", "DropTableSpaceStmt", "AlterTableSpaceOptionsStmt", "AlterSystemStmt", "VariableSetStmt", "DropOwnedStmt", "ReassignOwnedStmt",
				"LoadStmt", "CreateSubscriptionStmt", "AlterSubscriptionStmt", "DropSubscriptionStmt":
				return fmt.Errorf("release SQL forbids %s: only database-local changes without transaction control are allowed", key)
			case "subtype":
				if value == "AT_SetTableSpace" {
					return fmt.Errorf("release SQL must not change tablespaces")
				}
			case "tablespacename", "tablespace":
				if text, ok := value.(string); ok && text != "" {
					return fmt.Errorf("release SQL must not change tablespaces")
				}
			case "is_program":
				if value == true {
					return fmt.Errorf("release SQL must not execute external programs")
				}
			}
			switch key {
			case "objectType", "removeType", "renameType", "objtype":
				switch value {
				case "OBJECT_DATABASE", "OBJECT_ROLE", "OBJECT_TABLESPACE":
					return fmt.Errorf("release SQL must not modify %s", value)
				}
			}
			if err := validateSQLNode(value); err != nil {
				return err
			}
		}
	}
	return nil
}
