package auth

import (
	"os"

	"github.com/bwmarrin/discordgo"
)

func CanInteract(user *discordgo.User) bool {
	allowlist := os.Getenv("DISCORD_USERNAME")

	if allowlist == "" || allowlist == (user.Username+"#"+user.Discriminator) {
		return true
	}

	return false
}
