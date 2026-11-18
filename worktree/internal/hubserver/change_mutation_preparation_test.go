package hubserver

type changeWriterMeasurementKey struct{}

type changeWriterMeasurement struct {
	statements int
	content    int
	active     bool
}
