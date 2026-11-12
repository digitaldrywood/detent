package hubserver

import (
	"encoding/json"
)

func collaborationPayloadColumn(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "CASE WHEN " + alias + "payload_version=1 THEN 'event:' || " + alias + "id ELSE " + alias + "data_json END"
}

func schedulerPayloadArchive(scope nativeScope, raw string) ([]byte, error) {
	return json.Marshal(storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: "scheduler.decision", Data: json.RawMessage(raw)})
}
