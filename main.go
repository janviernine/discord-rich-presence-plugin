// Discord Rich Presence Plugin for Navidrome
//
// This plugin integrates Navidrome with Discord Rich Presence. It shows how a plugin can
// keep a real-time connection to an external service while remaining completely stateless.
//
// Capabilities: Scrobbler, SchedulerCallback, WebSocketCallback
//
// NOTE: This plugin is for demonstration purposes only. It relies on the user's Discord
// token being stored in the Navidrome configuration file, which is not secure and may be
// against Discord's terms of service. Use it at your own risk.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scheduler"
	"github.com/navidrome/navidrome/plugins/pdk/go/scrobbler"
	"github.com/navidrome/navidrome/plugins/pdk/go/websocket"
)

// Configuration keys
const (
	clientIDKey             = "clientid"
	usersKey                = "users"
	activityNameKey         = "activityname"
	activityNameTemplateKey = "activitynametemplate"
	spotifyLinksKey         = "spotifylinks"
	caaEnabledKey           = "caaenabled"
	uguuEnabledKey          = "uguuenabled"
	clearOnPauseKey         = "clearonpause"
)

const (
	navidromeWebsiteURL = "https://www.navidrome.org"

	// navidromeLogoURL is used as fallback large image when track artwork is unavailable.
	navidromeLogoURL = "https://raw.githubusercontent.com/navidrome/website/refs/heads/master/assets/icons/logo.webp"

	pauseIconURL = "https://raw.githubusercontent.com/navidrome/discord-rich-presence-plugin/800bfacfb8e85c33692373b10ddbd27388f262d2/assets/pause.png"
)

// Playback states from PlaybackReportRequest.State
const (
	statePlaying = "playing"
	statePaused  = "paused"
	stateStopped = "stopped"
	stateExpired = "expired"
)

// Activity name display options
const (
	activityNameDefault = "Default"
	activityNameTrack   = "Track"
	activityNameArtist  = "Artist"
	activityNameAlbum   = "Album"
	activityNameCustom  = "Custom"
)

// userToken represents a user-token mapping from the config
type userToken struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// discordPlugin implements the scrobbler and scheduler interfaces.
type discordPlugin struct{}

// rpc handles Discord gateway communication (via websockets).
var rpc = &discordRPC{}

// init registers the plugin capabilities
func init() {
	scrobbler.Register(&discordPlugin{})
	scheduler.Register(&discordPlugin{})
	websocket.Register(rpc)
}

// getConfig loads the plugin configuration.
func getConfig() (clientID string, users map[string]string, err error) {
	clientID, ok := pdk.GetConfig(clientIDKey)
	if !ok || clientID == "" {
		pdk.Log(pdk.LogWarn, "missing ClientID in configuration")
		return "", nil, nil
	}

	// Get the users array from config
	usersJSON, ok := pdk.GetConfig(usersKey)
	if !ok || usersJSON == "" {
		pdk.Log(pdk.LogWarn, "no users configured")
		return clientID, nil, nil
	}

	// Parse the JSON array
	var userTokens []userToken
	if err := json.Unmarshal([]byte(usersJSON), &userTokens); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("failed to parse users config: %v", err))
		return clientID, nil, nil
	}

	if len(userTokens) == 0 {
		pdk.Log(pdk.LogWarn, "no users configured")
		return clientID, nil, nil
	}

	// Build the users map
	users = make(map[string]string)
	for _, ut := range userTokens {
		if ut.Username != "" && ut.Token != "" {
			users[ut.Username] = ut.Token
		}
	}

	if len(users) == 0 {
		pdk.Log(pdk.LogWarn, "no valid users configured")
		return clientID, nil, nil
	}

	return clientID, users, nil
}

// ============================================================================
// Scrobbler Implementation
// ============================================================================

// IsAuthorized checks if a user is authorized for Discord Rich Presence.
func (p *discordPlugin) IsAuthorized(input scrobbler.IsAuthorizedRequest) (bool, error) {
	_, users, err := getConfig()
	if err != nil {
		return false, fmt.Errorf("failed to check user authorization: %w", err)
	}

	_, authorized := users[input.Username]
	pdk.Log(pdk.LogInfo, fmt.Sprintf("IsAuthorized for user %s: %v", input.Username, authorized))
	return authorized, nil
}

// NowPlaying is a no-op — playback state is handled by PlaybackReport.
func (p *discordPlugin) NowPlaying(_ scrobbler.NowPlayingRequest) error {
	return nil
}

// Scrobble handles scrobble requests (no-op for Discord).
func (p *discordPlugin) Scrobble(_ scrobbler.ScrobbleRequest) error {
	// Discord Rich Presence doesn't need scrobble events
	return nil
}

// PlaybackReport handles playback state reports from Navidrome.
func (p *discordPlugin) PlaybackReport(input scrobbler.PlaybackReportRequest) error {
	pdk.Log(pdk.LogDebug, fmt.Sprintf("PlaybackReport request: %s", formatRequest(input)))
	switch input.State {
	case statePlaying:
		return p.handlePlayingOrPaused(input)
	case statePaused:
		return p.handlePlayingOrPaused(input)
	case stateStopped, stateExpired:
		return p.handleStopped(input)
	default:
		return nil
	}
}

func formatRequest(input scrobbler.PlaybackReportRequest) string {
	return fmt.Sprintf("state=%s user=%s track=%q artist=%q pos=%dms rate=%.1f player=%s",
		input.State, input.Username, input.Track.Title, input.Track.Artist,
		input.PositionMs, input.PlaybackRate, input.PlayerName)
}

func (p *discordPlugin) handlePlayingOrPaused(input scrobbler.PlaybackReportRequest) error {
	paused := input.State == statePaused

	// [DBG-PHANTOM] Temporary tracer while diagnosing phantom track flashes
	// (e.g. a stale Popstar showing between switches). TODO: remove after diagnosis.
	pdk.Log(pdk.LogInfo, fmt.Sprintf("[DBG-PHANTOM] user=%s state=%s track=%q trackID=%q pos=%dms ts=%d rate=%.1f player=%s playerID=%s",
		input.Username, input.State, input.Track.Title, input.Track.ID,
		input.PositionMs, input.Timestamp, input.PlaybackRate,
		input.PlayerName, input.PlayerId))

	anchor := loadAnchor(input.Username)

	// Guard 1 (recency): drop playing/paused reports for a track that a newer
	// report already superseded. Late or out-of-order duplicates must not
	// flash over the current presence.
	if anchor != nil && input.Track.ID != "" && anchor.TrackID != "" &&
		input.Track.ID != anchor.TrackID && input.Timestamp < anchor.LastTs {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Ignoring stale report for user %s: track %q (ts=%d) is older than shown track (lastTs=%d)",
			input.Username, input.Track.Title, input.Timestamp, anchor.LastTs))
		return nil
	}

	clientID, userToken, err := connectUser(input.Username)
	if err != nil {
		return err
	}

	rate := input.PlaybackRate
	if rate <= 0 {
		rate = 1.0
	}

	// Convert track-time position to wall-clock elapsed time
	wallElapsedMs := int64(float64(input.PositionMs) / rate)
	wallDurationMs := int64(float64(int64(input.Track.Duration)*1000) / rate)

	fresh := activityTimestamps{
		Start: input.Timestamp*1000 - wallElapsedMs,
		End:   input.Timestamp*1000 - wallElapsedMs + wallDurationMs,
	}

	clearOnPauseEnabled, _ := pdk.GetConfig(clearOnPauseKey)
	clearOnPause := clearOnPauseEnabled == "true"

	if clearOnPause && paused {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Clearing presence for user %s, track: %s (paused=%v, clearOnPause=%v)", input.Username, input.Track.Title, paused, clearOnPause))
		_, pausedAnchor := resolveTimestamps(input, fresh, anchor)
		pausedAnchor.Paused = true
		if pausedAnchor.PauseTs <= 0 {
			pausedAnchor.PauseTs = input.Timestamp
		}
		storeAnchor(input.Username, pausedAnchor, input.Track)
		return rpc.clearActivity(input.Username)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Setting presence for user %s, track: %s (paused=%v)", input.Username, input.Track.Title, paused))

	activityName, statusDisplayType := resolveActivityName(input.Track)

	spotifyURL, artistSearchURL := resolveSpotifyLinks(input.Track)


	// Guard 2 (anchor): keep the original clock for the same track instead of
	// re-anchoring from every report. Mid-track reports frequently carry stale
	// or zero positions, which would shove Start forward and leave Discord
	// behind the real playback.
	ts, newAnchor := resolveTimestamps(input, fresh, anchor)
	storeAnchor(input.Username, newAnchor, input.Track)

	assets := activityAssets{
		LargeImage: getImageURL(input.Username, input.Track),
		LargeText:  input.Track.Album,
		LargeURL:   spotifyURL,
	}

	if paused {
		ts = activityTimestamps{Start: newAnchor.PauseTs * 1000}
		assets.SmallImage = pauseIconURL
		assets.SmallText = "Paused"
	}

	return rpc.sendActivity(clientID, input.Username, userToken, activity{
		Application:       clientID,
		Name:              activityName,
		Type:              2,
		Details:           input.Track.Title,
		DetailsURL:        spotifyURL,
		State:             displayArtists(input.Track),
		StateURL:          artistSearchURL,
		StatusDisplayType: statusDisplayType,
		Timestamps:        ts,
		Assets:            assets,
	})
}

func (p *discordPlugin) handleStopped(input scrobbler.PlaybackReportRequest) error {
	// Guard 3: a late stop for a superseded track must not clear the current
	// presence (nor tear down its gateway connection).
	if anchor := loadAnchor(input.Username); anchor != nil && input.Track.ID != "" &&
		anchor.TrackID != "" && input.Track.ID != anchor.TrackID {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Ignoring stale stop for user %s: track %q is not the shown track",
			input.Username, input.Track.Title))
		return nil
	}
	dropAnchor(input.Username)

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Clearing presence for user %s", input.Username))

	clearErr := rpc.clearActivity(input.Username)
	disconnectErr := rpc.disconnect(input.Username)

	if clearErr != nil {
		return fmt.Errorf("failed to clear activity: %w", clearErr)
	}
	if disconnectErr != nil {
		return fmt.Errorf("failed to disconnect from Discord: %w", disconnectErr)
	}
	return nil
}

func connectUser(username string) (clientID, token string, err error) {
	clientID, users, err := getConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to get config: %w", err)
	}
	if clientID == "" {
		return "", "", fmt.Errorf("missing ClientID in configuration")
	}

	token, authorized := users[username]
	if !authorized {
		return "", "", fmt.Errorf("%w: user '%s' not authorized", scrobbler.ScrobblerErrorNotAuthorized, username)
	}

	if err := rpc.connect(username, token); err != nil {
		return "", "", fmt.Errorf("failed to connect to Discord: %w", err)
	}
	return clientID, token, nil
}

// displayArtists returns the track artists as "Artist1, Artist2", matching Feishin's Discord presence. It prefers Navidrome's structured Artists list (split from "feat."/"ft." tags at scan time) and falls back to the raw Artist tag when the list is empty.
func displayArtists(track scrobbler.TrackInfo) string {
	if len(track.Artists) == 0 {
		return track.Artist
	}
	names := make([]string, len(track.Artists))
	for i, a := range track.Artists {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

// presenceAnchor is the last presence clock shown for a user. Plugin
// instances are stateless across calls, so it lives in the host cache.
type presenceAnchor struct {
	TrackID string `json:"trackId"`
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
	LastTs  int64  `json:"lastTs"`
	Paused  bool   `json:"paused"`
	PauseTs int64  `json:"pauseTs"`
}

func anchorKey(username string) string {
	return "discord.anchor." + username
}

func anchorCacheTTL(track scrobbler.TrackInfo) int64 {
	ttl := int64(track.Duration) + 1800
	if ttl < 1800 {
		ttl = 1800
	}
	return ttl
}

func loadAnchor(username string) *presenceAnchor {
	raw, ok, err := host.CacheGetString(anchorKey(username))
	if err != nil || !ok || raw == "" {
		return nil
	}
	var a presenceAnchor
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil
	}
	return &a
}

func storeAnchor(username string, a *presenceAnchor, track scrobbler.TrackInfo) {
	raw, err := json.Marshal(a)
	if err != nil {
		return
	}
	_ = host.CacheSetString(anchorKey(username), string(raw), anchorCacheTTL(track))
}

func dropAnchor(username string) {
	_ = host.CacheRemove(anchorKey(username))
}

// resolveTimestamps picks the presence clock for a report. Same-track reports
// reuse the stored anchor (immune to stale/zero mid-track positions); a resume
// shifts the stored clock by the paused duration instead of trusting the
// reported position. Anything else anchors fresh from the report.
func resolveTimestamps(input scrobbler.PlaybackReportRequest, fresh activityTimestamps, anchor *presenceAnchor) (activityTimestamps, *presenceAnchor) {
	paused := input.State == statePaused
	durationMs := int64(input.Track.Duration) * 1000
	next := &presenceAnchor{TrackID: input.Track.ID, LastTs: input.Timestamp}
	if anchor != nil && anchor.LastTs > next.LastTs {
		next.LastTs = anchor.LastTs
	}
	sameTrack := anchor != nil && anchor.TrackID != "" && anchor.TrackID == input.Track.ID
	anchorStale := sameTrack && input.Timestamp*1000-anchor.StartMs > durationMs+5*60*1000
	if sameTrack && !anchorStale {
		next.StartMs, next.EndMs = anchor.StartMs, anchor.EndMs
		switch {
		case anchor.Paused && !paused:
			shift := (input.Timestamp - anchor.PauseTs) * 1000
			if shift < 0 {
				shift = 0
			}
			next.StartMs += shift
			next.EndMs += shift
			pdk.Log(pdk.LogInfo, fmt.Sprintf("Resuming presence for user %s, track: %s (shifted clock by %dms of pause)",
				input.Username, input.Track.Title, shift))
		case paused:
			next.Paused = true
			next.PauseTs = anchor.PauseTs
			if !anchor.Paused || next.PauseTs <= 0 {
				next.PauseTs = input.Timestamp
			}
		}
		return activityTimestamps{Start: next.StartMs, End: next.EndMs}, next
	}
	next.StartMs, next.EndMs = fresh.Start, fresh.End
	next.Paused = paused
	if paused {
		next.PauseTs = input.Timestamp
	}
	return fresh, next
}

func resolveActivityName(track scrobbler.TrackInfo) (string, int) {
	activityNameOption, _ := pdk.GetConfig(activityNameKey)
	switch activityNameOption {
	case activityNameTrack:
		return track.Title, statusDisplayName
	case activityNameAlbum:
		return track.Album, statusDisplayName
	case activityNameArtist:
		return displayArtists(track), statusDisplayName
	case activityNameCustom:
		template, _ := pdk.GetConfig(activityNameTemplateKey)
		if template != "" {
			artists := displayArtists(track)
			r := strings.NewReplacer(
				"{track}", track.Title,
				"{artist}", track.Artist,
				"{artists}", artists,
				"{album}", track.Album,
			)
			return r.Replace(template), statusDisplayName
		}
	}
	return "Navidrome", statusDisplayDetails
}

func resolveSpotifyLinks(track scrobbler.TrackInfo) (string, string) {
	spotifyLinksOption, _ := pdk.GetConfig(spotifyLinksKey)
	if spotifyLinksOption != "true" {
		return "", ""
	}
	return resolveSpotifyURL(track), spotifySearchURL(track.Artist)
}

// ============================================================================
// Scheduler Callback Implementation
// ============================================================================

// OnCallback handles scheduler callbacks.
func (p *discordPlugin) OnCallback(input scheduler.SchedulerCallbackRequest) error {
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Scheduler callback: id=%s, payload=%s, recurring=%v", input.ScheduleID, input.Payload, input.IsRecurring))

	switch input.Payload {
	case payloadHeartbeat:
		if err := rpc.handleHeartbeatCallback(input.ScheduleID); err != nil {
			return err
		}
	default:
		pdk.Log(pdk.LogWarn, fmt.Sprintf("Unknown scheduler callback payload: %s", input.Payload))
	}

	return nil
}

func main() {}
