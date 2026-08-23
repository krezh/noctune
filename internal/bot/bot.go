// Package bot wires noctune into Discord: it registers slash commands and
// turns each interaction into calls against the shared player.Manager, the
// same one internal/api drives from the web GUI.
package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/thomas-vilte/dave-go/session"

	"github.com/krezh/noctune/internal/config"
	"github.com/krezh/noctune/internal/player"
	"github.com/krezh/noctune/internal/resolve"
)

type Bot struct {
	Client   *bot.Client
	players  *player.Manager
	resolver *resolve.Resolver
	cfg      *config.Config
}

// NewClient creates the underlying disgo bot.Client with the intents and
// DAVE (E2EE) session backend noctune needs. It's split out from New so
// callers can build player.Manager (which itself needs the client) before
// wiring the bot.
func NewClient(cfg *config.Config) (*bot.Client, error) {
	client, err := disgo.New(cfg.DiscordBotToken,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildVoiceStates),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(session.CreateFunc()),
		),
		bot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagGuilds, cache.FlagChannels, cache.FlagVoiceStates),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create discord client: %w", err)
	}
	return client, nil
}

func New(cfg *config.Config, client *bot.Client, players *player.Manager, resolver *resolve.Resolver) *Bot {
	b := &Bot{
		Client:   client,
		players:  players,
		resolver: resolver,
		cfg:      cfg,
	}
	client.EventManager.AddEventListeners(&events.ListenerAdapter{
		OnReady:                         b.onReady,
		OnApplicationCommandInteraction: b.onInteraction,
	})
	return b
}

func (b *Bot) Open() error {
	return b.Client.OpenGateway(context.Background())
}

func (b *Bot) Close() error {
	b.Client.Close(context.Background())
	return nil
}

func (b *Bot) onReady(event *events.Ready) {
	log.Printf("noctune: logged in as %s", event.User.Username)
	commands := commandDefinitions()
	var err error
	if b.cfg.DiscordTestGuildID != "" {
		guildID, perr := snowflake.Parse(b.cfg.DiscordTestGuildID)
		if perr != nil {
			log.Printf("noctune: register slash commands: %v", perr)
			return
		}
		_, err = event.Client().Rest.SetGuildCommands(event.Client().ApplicationID, guildID, commands)
	} else {
		_, err = event.Client().Rest.SetGlobalCommands(event.Client().ApplicationID, commands)
	}
	if err != nil {
		log.Printf("noctune: register slash commands: %v", err)
	}
}

func (b *Bot) onInteraction(event *events.ApplicationCommandInteractionCreate) {
	if event.GuildID() == nil || event.Member() == nil {
		b.reply(event, "noctune only works inside a server.")
		return
	}
	guildID := event.GuildID().String()

	data := event.SlashCommandInteractionData()
	switch data.CommandName() {
	case "play":
		b.handlePlay(event, guildID, data)
	case "skip":
		b.handleSkip(event, guildID)
	case "pause":
		b.handlePause(event, guildID)
	case "resume":
		b.handleResume(event, guildID)
	case "stop":
		b.handleStop(event, guildID)
	case "leave":
		b.handleLeave(event, guildID)
	case "queue":
		b.handleQueue(event, guildID)
	case "nowplaying":
		b.handleNowPlaying(event, guildID)
	case "volume":
		b.handleVolume(event, guildID, data)
	case "loop":
		b.handleLoop(event, guildID, data)
	}
}

func (b *Bot) handlePlay(event *events.ApplicationCommandInteractionCreate, guildID string, data discord.SlashCommandInteractionData) {
	query := data.String("query")

	if err := event.DeferCreateMessage(false); err != nil {
		log.Printf("noctune: defer /play response: %v", err)
		return
	}

	vs, ok := b.Client.Caches.VoiceState(*event.GuildID(), event.User().ID)
	if !ok || vs.ChannelID == nil {
		b.followup(event, "Join a voice channel first.")
		return
	}

	channelID := vs.ChannelID.String()
	gp := b.players.Get(guildID)

	// Run blocking work (voice join + resolve) off the event-dispatch goroutine.
	// conn.Open waits for gateway events (voice state update, voice server
	// update) that can never arrive if we block the event loop.
	go func() {
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer joinCancel()
		if err := gp.Join(joinCtx, channelID); err != nil {
			b.followup(event, fmt.Sprintf("Couldn't join your voice channel: %v", err))
			return
		}

		// AvatarURL, not EffectiveAvatarURL: a user with no custom avatar set
		// falls back to a differently-shaped Discord "default avatar" CDN
		// URL that discordAvatarURLPattern (avatarcache.go) doesn't match,
		// since only real per-user avatar URLs are ever worth caching. nil
		// here just means no avatar shown, same as the web session path.
		var avatarURL string
		if u := event.User().AvatarURL(); u != nil {
			avatarURL = *u
		}
		tracks, err := b.resolver.Resolve(context.Background(), query, event.User().Username, avatarURL)
		if err != nil || len(tracks) == 0 {
			b.followup(event, fmt.Sprintf("Couldn't find anything for %q.", query))
			return
		}
		for _, t := range tracks {
			if err := gp.Enqueue(t); err != nil {
				b.followup(event, err.Error())
				return
			}
		}

		var embed discord.Embed
		if len(tracks) == 1 {
			embed = trackEmbed(tracks[0]).WithAuthorName("Added to queue")
		} else {
			embed = discord.NewEmbed().
				WithColor(embedColor).
				WithAuthorName(fmt.Sprintf("Added %d tracks to queue", len(tracks))).
				WithTitle(tracks[0].Title).
				WithDescription(tracks[0].Artist)
			if tracks[0].SourceURL != "" {
				embed = embed.WithURL(tracks[0].SourceURL)
			}
			if tracks[0].ArtworkURL != "" {
				embed = embed.WithThumbnail(tracks[0].ArtworkURL)
			}
			if tracks[0].RequestedBy != "" {
				embed = embed.AddField("Requested by", tracks[0].RequestedBy, true)
			}
		}
		if avatarURL != "" {
			embed = embed.WithAuthorIcon(avatarURL)
		}
		if link := b.webGUILink(guildID); link != "" {
			embed = embed.AddField("Web", "[Open noctune]("+link+")", true)
		}
		b.followupEmbed(event, embed)
	}()
}

// webGUILink returns the web GUI's URL for a guild, or "" if
// WEB_PUBLIC_URL isn't configured.
func (b *Bot) webGUILink(guildID string) string {
	if b.cfg.WebPublicURL == "" {
		return ""
	}
	return strings.TrimRight(b.cfg.WebPublicURL, "/") + "/g/" + guildID
}

func (b *Bot) handleSkip(event *events.ApplicationCommandInteractionCreate, guildID string) {
	msg := "Skipped."
	if err := b.players.Get(guildID).Skip(); err != nil {
		msg = err.Error()
	}
	b.reply(event, msg)
}

func (b *Bot) handlePause(event *events.ApplicationCommandInteractionCreate, guildID string) {
	msg := "Paused."
	if err := b.players.Get(guildID).Pause(); err != nil {
		msg = err.Error()
	}
	b.reply(event, msg)
}

func (b *Bot) handleResume(event *events.ApplicationCommandInteractionCreate, guildID string) {
	msg := "Resumed."
	if err := b.players.Get(guildID).Resume(); err != nil {
		msg = err.Error()
	}
	b.reply(event, msg)
}

func (b *Bot) handleStop(event *events.ApplicationCommandInteractionCreate, guildID string) {
	_ = b.players.Get(guildID).Stop()
	b.reply(event, "Stopped and cleared the queue.")
}

func (b *Bot) handleLeave(event *events.ApplicationCommandInteractionCreate, guildID string) {
	_ = b.players.Get(guildID).Leave()
	b.reply(event, "Left the voice channel.")
}

func (b *Bot) handleQueue(event *events.ApplicationCommandInteractionCreate, guildID string) {
	st := b.players.Get(guildID).Snapshot()
	e := discord.NewEmbed().WithColor(embedColor).WithTitle("Queue")
	var sb strings.Builder
	if st.Current != nil {
		if st.Current.SourceURL != "" {
			fmt.Fprintf(&sb, "**Now playing:** [%s](%s) — %s\n\n", st.Current.Title, st.Current.SourceURL, st.Current.Artist)
		} else {
			fmt.Fprintf(&sb, "**Now playing:** %s — %s\n\n", st.Current.Title, st.Current.Artist)
		}
		if st.Current.ArtworkURL != "" {
			e = e.WithThumbnail(st.Current.ArtworkURL)
		}
	} else {
		sb.WriteString("Nothing is playing.\n\n")
	}
	if len(st.Queue) == 0 {
		sb.WriteString("Queue is empty.")
	} else {
		sb.WriteString("**Up next:**\n")
		n := min(len(st.Queue), 10)
		for idx, t := range st.Queue[:n] {
			fmt.Fprintf(&sb, "%d. %s — %s\n", idx+1, t.Title, t.Artist)
		}
		if len(st.Queue) > n {
			fmt.Fprintf(&sb, "…and %d more", len(st.Queue)-n)
		}
	}
	b.replyEmbed(event, e.WithDescription(sb.String()))
}

func (b *Bot) handleNowPlaying(event *events.ApplicationCommandInteractionCreate, guildID string) {
	st := b.players.Get(guildID).Snapshot()
	if st.Current == nil {
		b.reply(event, "Nothing is playing.")
		return
	}
	t := st.Current
	e := trackEmbed(t).WithAuthorName("Now Playing")
	if t.Duration > 0 && st.Position > 0 {
		pos := fmtDuration(st.Position.Round(time.Second)) + " / " + fmtDuration(t.Duration)
		e = e.WithField(0, "Duration", pos, true)
	}
	e = e.AddField("Status", string(st.Status), true).
		AddField("Volume", fmt.Sprintf("%d%%", st.Volume), true)
	b.replyEmbed(event, e)
}

func (b *Bot) handleVolume(event *events.ApplicationCommandInteractionCreate, guildID string, data discord.SlashCommandInteractionData) {
	level := data.Int("level")
	if err := b.players.Get(guildID).SetVolume(level); err != nil {
		b.reply(event, err.Error())
		return
	}
	b.reply(event, fmt.Sprintf("Volume set to %d%%.", level))
}

func (b *Bot) handleLoop(event *events.ApplicationCommandInteractionCreate, guildID string, data discord.SlashCommandInteractionData) {
	mode := player.LoopMode(data.String("mode"))
	b.players.Get(guildID).SetLoop(mode)
	b.reply(event, fmt.Sprintf("Loop mode: %s", mode))
}

const embedColor = 0x5865F2

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func trackEmbed(t *player.Track) discord.Embed {
	desc := t.Artist
	if t.Album != "" && t.Album != t.Artist && t.Album != t.Title {
		desc += " — " + t.Album
	}
	e := discord.NewEmbed().
		WithColor(embedColor).
		WithTitle(t.Title).
		WithDescription(desc)
	if t.SourceURL != "" {
		e = e.WithURL(t.SourceURL)
	}
	if t.ArtworkURL != "" {
		e = e.WithThumbnail(t.ArtworkURL)
	}
	if t.Duration > 0 {
		e = e.AddField("Duration", fmtDuration(t.Duration), true)
	}
	if t.RequestedBy != "" {
		e = e.AddField("Requested by", t.RequestedBy, true)
	}
	return e
}

func (b *Bot) reply(event *events.ApplicationCommandInteractionCreate, content string) {
	if err := event.CreateMessage(discord.NewMessageCreate().WithContent(content)); err != nil {
		log.Printf("noctune: respond to interaction: %v", err)
	}
}

func (b *Bot) replyEmbed(event *events.ApplicationCommandInteractionCreate, embed discord.Embed) {
	if err := event.CreateMessage(discord.NewMessageCreate().AddEmbeds(embed)); err != nil {
		log.Printf("noctune: respond to interaction: %v", err)
	}
}

func (b *Bot) followup(event *events.ApplicationCommandInteractionCreate, content string) {
	if _, err := event.Client().Rest.CreateFollowupMessage(event.ApplicationID(), event.Token(), discord.NewMessageCreate().WithContent(content)); err != nil {
		log.Printf("noctune: followup message: %v", err)
	}
}

func (b *Bot) followupEmbed(event *events.ApplicationCommandInteractionCreate, embed discord.Embed) {
	if _, err := event.Client().Rest.CreateFollowupMessage(event.ApplicationID(), event.Token(), discord.NewMessageCreate().AddEmbeds(embed)); err != nil {
		log.Printf("noctune: followup message: %v", err)
	}
}

func commandDefinitions() []discord.ApplicationCommandCreate {
	return []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:        "play",
			Description: "Queue a search, a YouTube link, or a Spotify track/album/playlist link",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "query",
					Description: "Search text, YouTube URL, or Spotify link",
					Required:    true,
				},
			},
		},
		discord.SlashCommandCreate{Name: "skip", Description: "Skip the current track"},
		discord.SlashCommandCreate{Name: "pause", Description: "Pause playback"},
		discord.SlashCommandCreate{Name: "resume", Description: "Resume playback"},
		discord.SlashCommandCreate{Name: "stop", Description: "Stop playback and clear the queue"},
		discord.SlashCommandCreate{Name: "leave", Description: "Leave the voice channel"},
		discord.SlashCommandCreate{Name: "queue", Description: "Show the current queue"},
		discord.SlashCommandCreate{Name: "nowplaying", Description: "Show the currently playing track"},
		discord.SlashCommandCreate{
			Name:        "volume",
			Description: "Set playback volume",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionInt{
					Name:        "level",
					Description: "0-100, 100 is normal",
					Required:    true,
					MinValue:    &volumeMin,
					MaxValue:    &volumeMax,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "loop",
			Description: "Set loop mode",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "mode",
					Description: "off, track, or queue",
					Required:    true,
					Choices: []discord.ApplicationCommandOptionChoiceString{
						{Name: "off", Value: string(player.LoopOff)},
						{Name: "track", Value: string(player.LoopTrack)},
						{Name: "queue", Value: string(player.LoopQueue)},
					},
				},
			},
		},
	}
}

var volumeMin, volumeMax = 0, 100
