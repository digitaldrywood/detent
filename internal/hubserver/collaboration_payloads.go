package hubserver

func collaborationPayloadColumn(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "CASE WHEN " + alias + "payload_version=1 THEN 'event:' || " + alias + "id ELSE " + alias + "data_json END"
}
