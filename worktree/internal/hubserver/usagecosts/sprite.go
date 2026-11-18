package usagecosts

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SpriteRequest struct {
	tracker.Mutation
	Observations []Observation `json:"observations"`
}

func SpriteMetrics() []string {
	return []string{"cpu", "ram", "storage_hot", "storage_cold"}
}

func ValidateSpriteObservation(o *Observation, now time.Time) error {
	if o.Provider != "fly_sprites" || o.Bucket != SpriteBucket {
		return hubapi.PublicInvalid("Sprite usage requires the fly_sprites provider and sprite_infrastructure bucket")
	}
	unit := "GB-hour"
	if o.Metric == "cpu" {
		unit = "CPU-hour"
	}
	if !slices.Contains(SpriteMetrics(), o.Metric) || o.Unit != unit {
		return hubapi.PublicInvalid("Sprite quantities use cpu in CPU-hour or ram, storage_hot, storage_cold in GB-hour")
	}
	return Normalize(o, now)
}

func ValidateSpriteRequest(request *SpriteRequest, now time.Time) error {
	if len(request.Observations) == 0 || len(request.Observations) > 128 {
		return hubapi.PublicInvalid("Import between 1 and 128 Sprite usage observations")
	}
	for index := range request.Observations {
		if err := ValidateSpriteObservation(&request.Observations[index], now); err != nil {
			return err
		}
	}
	return nil
}
