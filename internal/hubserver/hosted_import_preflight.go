package hubserver

const hostedImportFallbackRows = 1000

type hostedImportFieldsKey struct{}

type hostedImportFieldSnapshot map[string]map[string]hostedImportFieldValues
