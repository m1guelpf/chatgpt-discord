package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"github.com/m1guelpf/chatgpt-discord/src/auth"
	"github.com/m1guelpf/chatgpt-discord/src/chatgpt"
	"github.com/m1guelpf/chatgpt-discord/src/config"
	"github.com/m1guelpf/chatgpt-discord/src/markdown"
	"github.com/m1guelpf/chatgpt-discord/src/ratelimit"
	"github.com/m1guelpf/chatgpt-discord/src/ref"
	"github.com/m1guelpf/chatgpt-discord/src/session"
)

type Conversation struct {
	ConversationID string
	LastMessageID  string
}

func main() {
	config, err := config.Init()
	if err != nil {
		log.Fatalf("Couldn't load config: %v", err)
	}

	if config.OpenAISession == "" {
		session, err := session.GetSession()
		if err != nil {
			log.Fatalf("Couldn't get OpenAI session: %v", err)
		}

		err = config.Set("OpenAISession", session)
		if err != nil {
			log.Fatalf("Couldn't save OpenAI session: %v", err)
		}
	}

	chatGPT := chatgpt.Init(config)
	log.Println("Started ChatGPT")

	err = godotenv.Load()
	if err != nil {
		log.Fatalf("Couldn't load .env file: %v", err)
	}

	discord, err := discordgo.New("Bot " + os.Getenv("DISCORD_TOKEN"))
	if err != nil {
		log.Fatalf("Couldn't start Discord bot: %v", err)
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		discord.Close()
		os.Exit(0)
	}()

	userConversations := make(map[string]Conversation)

	// get app id
	app, err := discord.Application("@me")
	if err != nil {
		log.Fatalf("Couldn't get app id: %v", err)
	}

	_, err = discord.ApplicationCommandCreate(app.ID, os.Getenv("DISCORD_GUILD_ID"), &discordgo.ApplicationCommand{
		Name:         "reload",
		Description:  "Start a new conversation.",
		DMPermission: ref.Of(true),
	})
	if err != nil {
		log.Fatalf("Couldn't create reload command: %v", err)
	}

	discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}

		if !auth.CanInteract(i.Member.User) {
			err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "You are not authorized to use this bot.",
				},
			})
			if err != nil {
				log.Printf("Couldn't send message: %v", err)
			}
			return
		}

		if i.ApplicationCommandData().Name == "reload" {
			userConversations[i.ChannelID] = Conversation{}
			err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Started a new conversation. Enjoy!",
				},
			})
		}
	})

	discord.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID {
			return
		}

		if m.GuildID != "" && ((len(m.Mentions) == 0 || m.Mentions[0].ID != s.State.User.ID) || (m.ReferencedMessage != nil && m.ReferencedMessage.Author.ID != s.State.User.ID)) {
			return
		}

		if !auth.CanInteract(m.Author) {
			_, err := s.ChannelMessageSendReply(m.ChannelID, "You are not authorized to use this bot.", &discordgo.MessageReference{MessageID: m.ID})
			if err != nil {
				log.Printf("Couldn't send message: %v", err)
			}
			return
		}

		query := strings.TrimSpace(strings.ReplaceAll(m.Content, fmt.Sprintf("<@%s>", s.State.User.ID), ""))

		feed, err := chatGPT.SendMessage(query, userConversations[m.ChannelID].ConversationID, userConversations[m.ChannelID].LastMessageID)
		if err != nil {
			_, err = s.ChannelMessageSendReply(m.ChannelID, fmt.Sprintf("Couldn't send message: %v", err), &discordgo.MessageReference{MessageID: m.ID})
			if err != nil {
				log.Printf("Couldn't send message: %v", err)
			}
		}

		err = s.ChannelTyping(m.ChannelID)
		if err != nil {
			log.Printf("Couldn't start typing: %v", err)
		}

		var msg discordgo.Message
		var lastResp string

		debouncedType := ratelimit.Debounce((10 * time.Second), func() {
			err = s.ChannelTyping(m.ChannelID)
			if err != nil {
				log.Printf("Couldn't start typing: %v", err)
			}
		})

		debouncedEdit := ratelimit.DebounceWithArgs((1 * time.Second), func(text interface{}, messageId interface{}) {
			_, err := s.ChannelMessageEdit(m.ChannelID, messageId.(string), text.(string))
			if err != nil {
				log.Printf("Couldn't edit message: %v", err)
			}
		})

	pollResponse:
		for {
			select {
			case response, ok := <-feed:
				if !ok {
					break pollResponse
				}

				userConversations[m.ChannelID] = Conversation{
					LastMessageID:  response.MessageId,
					ConversationID: response.ConversationId,
				}

				lastResp = markdown.EnsureFormatting(response.Message)

				if msg.ID == "" {
					_msg, err := s.ChannelMessageSendReply(m.ChannelID, lastResp, &discordgo.MessageReference{MessageID: m.ID})
					if err != nil {
						log.Printf("Couldn't send message: %v", err)
					}
					msg = *_msg
				} else {
					debouncedEdit(lastResp, msg.ID)
				}

				debouncedType()
			}
		}

		_, err = s.ChannelMessageEdit(m.ChannelID, msg.ID, lastResp)
		if err != nil {
			log.Printf("Couldn't perform final edit: %v", err)
		}
	})

	discord.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Println("Started Discord bot.")
		log.Printf("Add this bot to your server here: https://discord.com/api/oauth2/authorize?client_id=%s&permissions=2147484672&scope=bot%sapplications.commands", app.ID, "%20")
	})

	// start the bot
	err = discord.Open()
	if err != nil {
		log.Fatalf("Couldn't start Discord bot: %v", err)
	}

	<-make(chan struct{})
}
