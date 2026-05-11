package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OSRMClient handles communication with an OSRM server for map matching.
type OSRMClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewOSRMClient creates a new OSRM client. Returns nil if baseURL is empty (disabled).
func NewOSRMClient(baseURL string) *OSRMClient {
	if baseURL == "" {
		return nil
	}
	return &OSRMClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// DrivePosition represents a raw GPS position with timestamp for map matching.
type DrivePosition struct {
	Latitude  float64
	Longitude float64
	Timestamp time.Time
}

// MatchedPoint represents a single road-snapped coordinate.
type MatchedPoint struct {
	Latitude  float64
	Longitude float64
}

// osrmMatchResponse represents the JSON response from OSRM's match API.
type osrmMatchResponse struct {
	Code      string         `json:"code"`
	Matchings []osrmMatching `json:"matchings"`
}

type osrmMatching struct {
	Geometry   osrmGeometry `json:"geometry"`
	Confidence float64      `json:"confidence"`
}

type osrmGeometry struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"` // [lon, lat] pairs
}

// MatchDrive sends drive positions to the OSRM match API and returns road-snapped coordinates.
// Requires at least 2 positions. Returns nil, nil if matching is not possible (too few points).
func (c *OSRMClient) MatchDrive(ctx context.Context, positions []DrivePosition) ([]MatchedPoint, error) {
	if len(positions) < 2 {
		slog.Debug("too few positions for map matching", "count", len(positions))
		return nil, nil
	}

	// OSRM has a default limit of 100 waypoints per request.
	// Split into chunks if needed, with overlap to ensure continuity.
	const maxPerRequest = 100
	if len(positions) > maxPerRequest {
		return c.matchInChunks(ctx, positions, maxPerRequest)
	}

	return c.matchSingle(ctx, positions)
}

func (c *OSRMClient) matchSingle(ctx context.Context, positions []DrivePosition) ([]MatchedPoint, error) {
	// Build coordinates string: lon1,lat1;lon2,lat2;...
	coords := make([]string, len(positions))
	timestamps := make([]string, len(positions))
	for i, p := range positions {
		coords[i] = fmt.Sprintf("%f,%f", p.Longitude, p.Latitude)
		timestamps[i] = fmt.Sprintf("%d", p.Timestamp.Unix())
	}

	url := fmt.Sprintf("%s/match/v1/driving/%s?timestamps=%s&geometries=geojson&overview=full&gaps=ignore",
		c.baseURL,
		strings.Join(coords, ";"),
		strings.Join(timestamps, ";"),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating OSRM request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling OSRM match API: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading OSRM response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OSRM returned status %d: %s", resp.StatusCode, string(body))
	}

	var matchResp osrmMatchResponse
	if err := json.Unmarshal(body, &matchResp); err != nil {
		return nil, fmt.Errorf("parsing OSRM response: %w", err)
	}

	if matchResp.Code != "Ok" {
		return nil, fmt.Errorf("OSRM match failed with code: %s", matchResp.Code)
	}

	if len(matchResp.Matchings) == 0 {
		slog.Warn("OSRM returned no matchings")
		return nil, nil
	}

	// Collect all points from all matchings (there may be multiple if the trace was split)
	var result []MatchedPoint
	for _, matching := range matchResp.Matchings {
		slog.Debug("OSRM matching", "confidence", matching.Confidence, "points", len(matching.Geometry.Coordinates))
		for _, coord := range matching.Geometry.Coordinates {
			if len(coord) >= 2 {
				result = append(result, MatchedPoint{
					Latitude:  coord[1], // GeoJSON is [lon, lat]
					Longitude: coord[0],
				})
			}
		}
	}

	slog.Info("OSRM map matching complete", "input_points", len(positions), "matched_points", len(result))
	return result, nil
}

// matchInChunks splits large traces into overlapping chunks and merges results.
func (c *OSRMClient) matchInChunks(ctx context.Context, positions []DrivePosition, chunkSize int) ([]MatchedPoint, error) {
	var allPoints []MatchedPoint
	overlap := 5 // overlap points between chunks for continuity

	for start := 0; start < len(positions); {
		end := start + chunkSize
		if end > len(positions) {
			end = len(positions)
		}

		chunk := positions[start:end]
		matched, err := c.matchSingle(ctx, chunk)
		if err != nil {
			slog.Warn("OSRM chunk matching failed, skipping chunk", "start", start, "end", end, "error", err)
			start = end - overlap
			if start <= 0 {
				start = end
			}
			continue
		}

		if matched != nil {
			if len(allPoints) > 0 && start > 0 {
				// Skip first few points of subsequent chunks (overlap region)
				skipPoints := len(matched) / chunkSize * overlap
				if skipPoints > 0 && skipPoints < len(matched) {
					matched = matched[skipPoints:]
				}
			}
			allPoints = append(allPoints, matched...)
		}

		start = end - overlap
		if start <= 0 || end == len(positions) {
			break
		}
	}

	return allPoints, nil
}

// MatchAndStoreDrive fetches positions for a drive, runs OSRM map matching, and stores the result.
func MatchAndStoreDrive(ctx context.Context, pool *pgxpool.Pool, osrm *OSRMClient, driveID int) error {
	if osrm == nil {
		return nil
	}

	positions, err := GetDrivePositions(ctx, pool, driveID)
	if err != nil {
		return fmt.Errorf("getting drive positions: %w", err)
	}

	if len(positions) < 2 {
		slog.Debug("not enough positions for map matching", "drive_id", driveID, "positions", len(positions))
		return nil
	}

	matched, err := osrm.MatchDrive(ctx, positions)
	if err != nil {
		return fmt.Errorf("OSRM map matching: %w", err)
	}

	if matched == nil || len(matched) == 0 {
		slog.Warn("no matched points returned", "drive_id", driveID)
		return nil
	}

	if err := StoreMatchedPath(ctx, pool, driveID, matched); err != nil {
		return fmt.Errorf("storing matched path: %w", err)
	}

	slog.Info("drive map-matched successfully", "drive_id", driveID, "raw_points", len(positions), "matched_points", len(matched))
	return nil
}
