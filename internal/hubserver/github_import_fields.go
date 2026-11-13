package hubserver

const importedDependencyID = `COALESCE(r.dependency_id,(SELECT d.dependency_id FROM legacy_import_dependencies d WHERE d.sequence=r.sequence AND d.record_payload=r.record_json))`
