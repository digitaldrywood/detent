package reportdata

import "sync"

func ReadObjects[T, R any](records []T, read func(T) (R, error)) ([]R, []error) {
	values := make([]R, len(records))
	errs := make([]error, len(records))
	jobs := make(chan int, len(records))
	for index := range records {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(4, len(records)) {
		workers.Go(func() {
			for index := range jobs {
				values[index], errs[index] = read(records[index])
			}
		})
	}
	workers.Wait()
	return values, errs
}
