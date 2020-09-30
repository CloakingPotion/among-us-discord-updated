package discord

import (
	"github.com/denverquane/amongusdiscord/game"
	"log"

	"github.com/bwmarrin/discordgo"
)

//const voiceChannel = "758127642661748766"; // Cloaking's Server VoiceChannel
const voiceChannel = "758099224838668299";

func (guild *GuildState) handleGameEndMessage(s *discordgo.Session) {
	guild.AmongUsData.SetAllAlive()
	guild.AmongUsData.SetPhase(game.LOBBY)

	// apply the unmute/deafen to users who have state linked to them
	guild.handleTrackedMembers(s, 0, NoPriority)

	//clear the tracking and make sure all users are unlinked
	guild.clearGameTracking(s)

	// clear any existing game state message
	guild.AmongUsData.SetRoomRegion("", "")
}

func (guild *GuildState) handleGameStartMessage(s *discordgo.Session, m *discordgo.MessageCreate, room string, region string, channel TrackingChannel) {
	guild.AmongUsData.SetRoomRegion(room, region)

	guild.clearGameTracking(s)

	if channel.channelID != "" {
		guild.Tracking.AddTrackedChannel(channel.channelID, channel.channelName, channel.forGhosts)
	}

	guild.GameStateMsg.CreateMessage(s, gameStateResponse(guild), m.ChannelID)

	log.Println("Added self game state message")
}

func (guild *GuildState) createPrivateMapMessage(s *discordgo.Session, m *discordgo.MessageCreate) {

	// Custom Code:
	var guildId = m.GuildID;
	var g, _ = s.State.Guild(guildId)

	var idUsernameMap = make(map[string]string);

	for _, vs := range g.VoiceStates {
		if (vs.ChannelID != voiceChannel) {
			continue;
		}
		var member, err = s.State.Member(guildId, vs.UserID)

		if (err != nil) {
			log.Println("Error: " + err.Error());
			log.Println("Falling back to s.GuildMember implementation");
			member, _ = s.GuildMember(guildId, vs.UserID);
		}

		if (member == nil) {
			log.Print("Member is nil for vs.UserId: " + vs.UserID);
			continue;
		}

		var user = member.User
		var userID = user.ID

		var username = "";

		if member.Nick == "" {
			// Doesn't have a Nick.
			username = user.Username;
		} else {
			// Does have a nickname. Use nickname for reference
			username = member.Nick;
		}

		var targetUsername = idUsernameMap[userID];

		//log.Print("User: " + targetUsername + "|||||");
		//log.Print("User ID: " + user.ID);
		if (targetUsername == "") {
			log.Print("UserID hasn't been saved before: USERID:" + userID + " NAME:" + username);
			// user hasn't been saved before.
			//log.Print("User is equal to an empty string!");

			//log.Print("Descriminator: " + member.User.Discriminator);

			//TODO: CHANGE IMPLENTATION TO A MORE OPTIMIZED SOLUTION.
			//PERHAPS USE A CUSTOM OBJECT FOR THE VALUE TYPE OF THE MAP!
			//TRY TO MAKE NAME INFORMATION ACCESSIBLE EASILY

			var discriminationNecessary = false;

			for _, uName := range idUsernameMap {
				if (uName == username) {
					log.Print("Found a previous user with the same name as current user: " + username);
					log.Print("Setting discrimination necessary to true");
					//Came across exact same name. Update all with the same name to also include the discriminator for the name
					discriminationNecessary = true
					break;
				}
			}

			idUsernameMap[userID] = username;

			if (discriminationNecessary) {
				log.Print("Determined that discrimination necessary is true");
				usernameIdCopy := make(map[string]string);

				log.Print("Looking through the map for similar values.");
				for uID, uName := range idUsernameMap {
					var value = uName;
					if (uName == username) {
						log.Print("Found a similar value...");
						// Is a duplicate/original of the name. Add descriminator
						var targetMember, _ = s.State.Member(guildId, uID)
						value = uName + "#" + targetMember.User.Discriminator;
						log.Print("Updating name to use discriminator: " + targetMember.User.Discriminator);
					}

					usernameIdCopy[uID] = value;
				}
				log.Print("Finished iteration through map for similar values.");

				idUsernameMap = usernameIdCopy;

				log.Print("Updated map");


			}
		} else {
			log.Print("User has been saved before: " + user.Username);
			// User has been saved before
		}
	}

	guild.PrivateStateMsg.idUsernameMap = idUsernameMap;

	log.Print("Showing Map:")
	for uID, uName := range idUsernameMap {
		log.Print(uID + ": " + uName);
	}
	log.Print("Map Finished:")

	//var message *discordgo.Message;

	var message *discordgo.Message;
	for uID, uName := range idUsernameMap {
		message = guild.PrivateStateMsg.CreateMessage(s, guild.PrivateStateMsg.privateMapResponse(uID, uName), guild.PrivateStateMsg.privateChannelID)
		guild.PrivateStateMsg.printedUsers = append(guild.PrivateStateMsg.printedUsers, uID);
		break;
	}

	guild.PrivateStateMsg.message = message;


	log.Print("printing reactions:")

	for _, e := range guild.StatusEmojis[true] {
		guild.PrivateStateMsg.AddReaction(s, e.FormatForReaction())
		log.Print("Printed reaction...")
	}
	guild.PrivateStateMsg.AddReaction(s, "❌")
	log.Print("Reactions printed")
}

// sendMessage provides a single interface to send a message to a channel via discord
func sendMessage(s *discordgo.Session, channelID string, message string) *discordgo.Message {
	msg, err := s.ChannelMessageSend(channelID, message)
	if err != nil {
		log.Println(err)
	}
	return msg
}

func sendMessageEmbed(s *discordgo.Session, channelID string, message *discordgo.MessageEmbed) *discordgo.Message {
	msg, err := s.ChannelMessageSendEmbed(channelID, message)
	if err != nil {
		log.Println(err)
	}
	return msg
}

// editMessage provides a single interface to edit a message in a channel via discord
func editMessage(s *discordgo.Session, channelID string, messageID string, message string) *discordgo.Message {
	msg, err := s.ChannelMessageEdit(channelID, messageID, message)
	if err != nil {
		log.Println(err)
	}
	return msg
}

func editMessageEmbed(s *discordgo.Session, channelID string, messageID string, message *discordgo.MessageEmbed) *discordgo.Message {
	msg, err := s.ChannelMessageEditEmbed(channelID, messageID, message)
	if err != nil {
		log.Println(err)
	}
	return msg
}

func deleteMessage(s *discordgo.Session, channelID string, messageID string) {
	err := s.ChannelMessageDelete(channelID, messageID)
	if err != nil {
		log.Println(err)
	}
}

func addReaction(s *discordgo.Session, channelID, messageID, emojiID string) {
	err := s.MessageReactionAdd(channelID, messageID, emojiID)
	if err != nil {
		log.Println(err)
	}
}

func removeAllReactions(s *discordgo.Session, channelID, messageID string) {
	err := s.MessageReactionsRemoveAll(channelID, messageID)
	if err != nil {
		log.Println(err)
	}
}
