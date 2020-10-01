package discord

import (
	"container/heap"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/denverquane/amongusdiscord/game"
	"log"
	"sync"
	"time"
)

// GameDelays struct
type GameDelays struct {
	//maps from origin->new phases, with the integer number of seconds for the delay
	Delays map[game.PhaseNameString]map[game.PhaseNameString]int `json:"delays"`
}

func MakeDefaultDelays() GameDelays {
	return GameDelays{
		Delays: map[game.PhaseNameString]map[game.PhaseNameString]int{
			game.PhaseNames[game.LOBBY]: {
				game.PhaseNames[game.LOBBY]:   0,
				game.PhaseNames[game.TASKS]:   7,
				game.PhaseNames[game.DISCUSS]: 0,
			},
			game.PhaseNames[game.TASKS]: {
				game.PhaseNames[game.LOBBY]:   1,
				game.PhaseNames[game.TASKS]:   0,
				game.PhaseNames[game.DISCUSS]: 0,
			},
			game.PhaseNames[game.DISCUSS]: {
				game.PhaseNames[game.LOBBY]:   6,
				game.PhaseNames[game.TASKS]:   7,
				game.PhaseNames[game.DISCUSS]: 0,
			},
		},
	}
}

func (gd *GameDelays) GetDelay(origin, dest game.Phase) int {
	return gd.Delays[game.PhaseNames[origin]][game.PhaseNames[dest]]
}

// GuildState struct
type GuildState struct {
	PersistentGuildData *PersistentGuildData

	LinkCode string

	UserData UserDataSet
	Tracking Tracking

	GameStateMsg GameStateMessage
	PrivateStateMsg PrivateStateMessage

	StatusEmojis  AlivenessEmojis
	SpecialEmojis map[string]Emoji

	AmongUsData game.AmongUsData
}

type EmojiCollection struct {
	statusEmojis  AlivenessEmojis
	specialEmojis map[string]Emoji
	lock          sync.RWMutex
}

// TrackedMemberAction struct
type TrackedMemberAction struct {
	mute          bool
	move          bool
	message       string
	targetChannel Tracking
}

func (guild *GuildState) checkCacheAndAddUser(g *discordgo.Guild, s *discordgo.Session, userID string) (game.UserData, bool) {
	//check and see if they're cached first
	for _, v := range g.Members {
		if v.User.ID == userID {
			user := game.MakeUserDataFromDiscordUser(v.User, v.Nick)
			guild.UserData.AddFullUser(user)
			return user, true
		}
	}
	mem, err := s.GuildMember(guild.PersistentGuildData.GuildID, userID)
	if err != nil {
		log.Println(err)
		return game.UserData{}, false
	}
	user := game.MakeUserDataFromDiscordUser(mem.User, mem.Nick)
	guild.UserData.AddFullUser(user)
	return user, true
}

type HandlePriority int

const (
	NoPriority    HandlePriority = 0
	AlivePriority HandlePriority = 1
	DeadPriority  HandlePriority = 2
)

type PrioritizedPatchParams struct {
	priority    int
	patchParams UserPatchParameters
}

type PatchPriority []PrioritizedPatchParams

func (h PatchPriority) Len() int { return len(h) }

//NOTE this is inversed so HIGHER numbers are pulled FIRST
func (h PatchPriority) Less(i, j int) bool { return h[i].priority > h[j].priority }
func (h PatchPriority) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *PatchPriority) Push(x interface{}) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*h = append(*h, x.(PrioritizedPatchParams))
}

func (h *PatchPriority) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

//handleTrackedMembers moves/mutes players according to the current game state
func (guild *GuildState) handleTrackedMembers(dg *discordgo.Session, delay int, handlePriority HandlePriority) bool {

	g := guild.verifyVoiceStateChanges(dg)

	updateMade := false
	priorityQueue := &PatchPriority{}
	heap.Init(priorityQueue)

	for _, voiceState := range g.VoiceStates {

		userData, err := guild.UserData.GetUser(voiceState.UserID)
		if err != nil {
			//the user doesn't exist in our userdata cache; add them
			added := false
			userData, added = guild.checkCacheAndAddUser(g, dg, voiceState.UserID)
			if !added {
				continue
			}
		}

		tracked := guild.Tracking.IsTracked(voiceState.ChannelID)
		//only actually tracked if we're in a tracked channel AND linked to a player
		tracked = tracked && userData.IsLinked()
		shouldMute, shouldDeaf := guild.PersistentGuildData.VoiceRules.GetVoiceState(userData.IsAlive(), tracked, guild.AmongUsData.GetPhase())

		nick := userData.GetPlayerName()
		if !guild.PersistentGuildData.ApplyNicknames {
			nick = ""
		}

		//only issue a change if the user isn't in the right state already
		//nicksmatch can only be false if the in-game data is != nil, so the reference to .audata below is safe
		//check the userdata is linked here to not accidentally undeafen music bots, for example
		if userData.IsLinked() && shouldMute != voiceState.Mute || shouldDeaf != voiceState.Deaf || (nick != "" && userData.GetNickName() != userData.GetPlayerName()) {

			//only issue the req to discord if we're not waiting on another one
			if !userData.IsPendingVoiceUpdate() {
				//wait until it goes through
				userData.SetPendingVoiceUpdate(true)

				guild.UserData.UpdateUserData(voiceState.UserID, userData)
				priority := 0

				if handlePriority != NoPriority {
					if handlePriority == AlivePriority && userData.IsAlive() {
						priority++
					} else if handlePriority == DeadPriority && !userData.IsAlive() {
						priority++
					}
				}

				params := UserPatchParameters{guild.PersistentGuildData.GuildID, voiceState.UserID, shouldDeaf, shouldMute, nick}

				heap.Push(priorityQueue, PrioritizedPatchParams{
					priority:    priority,
					patchParams: params,
				})

				updateMade = true
			}

		} else {
			if shouldMute {
				log.Printf("Not muting %s because they're already muted\n", userData.GetUserName())
			} else {
				log.Printf("Not unmuting %s because they're already unmuted\n", userData.GetUserName())
			}
		}
	}
	wg := sync.WaitGroup{}
	waitForHigherPriority := false

	if delay > 0 {
		log.Printf("Sleeping for %d seconds before applying changes to users\n", delay)
		time.Sleep(time.Second * time.Duration(delay))
	}

	for priorityQueue.Len() > 0 {
		p := heap.Pop(priorityQueue).(PrioritizedPatchParams)

		if p.priority > 0 {
			waitForHigherPriority = true
			log.Printf("User %s has higher priority: %d\n", p.patchParams.UserID, p.priority)
		} else if waitForHigherPriority {
			//wait for all the other users to get muted/unmuted completely, first
			log.Println("Waiting for high priority user changes first")
			wg.Wait()
			waitForHigherPriority = false
		}

		wg.Add(1)
		go muteWorker(dg, &wg, p.patchParams)
	}
	wg.Wait()

	return updateMade
}

func muteWorker(s *discordgo.Session, wg *sync.WaitGroup, parameters UserPatchParameters) {
	guildMemberUpdate(s, parameters)
	wg.Done()
}

func (guild *GuildState) verifyVoiceStateChanges(s *discordgo.Session) *discordgo.Guild {
	g, err := s.State.Guild(guild.PersistentGuildData.GuildID)
	if err != nil {
		log.Println(err)
	}

	for _, voiceState := range g.VoiceStates {
		userData, err := guild.UserData.GetUser(voiceState.UserID)

		if err != nil {
			//the user doesn't exist in our userdata cache; add them
			added := false
			userData, added = guild.checkCacheAndAddUser(g, s, voiceState.UserID)
			if !added {
				continue
			}
		}

		tracked := guild.Tracking.IsTracked(voiceState.ChannelID)
		//only actually tracked if we're in a tracked channel AND linked to a player
		tracked = tracked && userData.IsLinked()
		mute, deaf := guild.PersistentGuildData.VoiceRules.GetVoiceState(userData.IsAlive(), tracked, guild.AmongUsData.GetPhase())
		if userData.IsPendingVoiceUpdate() && voiceState.Mute == mute && voiceState.Deaf == deaf {
			userData.SetPendingVoiceUpdate(false)

			guild.UserData.UpdateUserData(voiceState.UserID, userData)

			//log.Println("Successfully updated pendingVoice")
		}

	}
	return g

}

//voiceStateChange handles more edge-case behavior for users moving between voice channels, and catches when
//relevant discord api requests are fully applied successfully. Otherwise, we can issue multiple requests for
//the same mute/unmute, erroneously
func (guild *GuildState) voiceStateChange(s *discordgo.Session, m *discordgo.VoiceStateUpdate) {
	g := guild.verifyVoiceStateChanges(s)

	updateMade := false

	//fetch the userData from our userData data cache
	userData, err := guild.UserData.GetUser(m.UserID)
	if err != nil {
		//the user doesn't exist in our userdata cache; add them
		userData, _ = guild.checkCacheAndAddUser(g, s, m.UserID)
	}
	tracked := guild.Tracking.IsTracked(m.ChannelID)
	//only actually tracked if we're in a tracked channel AND linked to a player
	tracked = tracked && userData.IsLinked()
	mute, deaf := guild.PersistentGuildData.VoiceRules.GetVoiceState(userData.IsAlive(), tracked, guild.AmongUsData.GetPhase())
	//check the userdata is linked here to not accidentally undeafen music bots, for example
	if userData.IsLinked() && !userData.IsPendingVoiceUpdate() && (mute != m.Mute || deaf != m.Deaf) {
		userData.SetPendingVoiceUpdate(true)

		guild.UserData.UpdateUserData(m.UserID, userData)

		nick := userData.GetPlayerName()
		if !guild.PersistentGuildData.ApplyNicknames {
			nick = ""
		}

		go guildMemberUpdate(s, UserPatchParameters{m.GuildID, m.UserID, deaf, mute, nick})

		log.Println("Applied deaf/undeaf mute/unmute via voiceStateChange")

		updateMade = true
	}

	if updateMade {
		log.Println("Updating state message")
		guild.GameStateMsg.Edit(s, gameStateResponse(guild))
	}
}
func (guild *GuildState) handleReactionGameStartAdd(s *discordgo.Session, m *discordgo.MessageReactionAdd) {
	//TODO: Add code here to handle reactions in private chat

	g, err := s.State.Guild(guild.PersistentGuildData.GuildID)
	if err != nil {
		log.Println(err)
	}



	if guild.GameStateMsg.Exists() {

		//verify that the user is reacting to the state/status message
		if guild.GameStateMsg.IsReactionTo(m) {



			idMatched := false
			for color, e := range guild.StatusEmojis[true] {
				if e.ID == m.Emoji.ID {
					idMatched = true
					log.Printf("Player %s reacted with color %s", m.UserID, game.GetColorStringForInt(color))
					//the user doesn't exist in our userdata cache; add them

					_, added := guild.checkCacheAndAddUser(g, s, m.UserID)
					if !added {
						log.Println("No users found in Discord for userID " + m.UserID)
					}

					playerData := guild.AmongUsData.GetByColor(game.GetColorStringForInt(color))
					if playerData != nil {
						guild.UserData.UpdatePlayerData(m.UserID, playerData)
					} else {
						log.Println("I couldn't find any player data for that color; is your capture linked?")
					}

					//then remove the player's reaction if we matched, or if we didn't
					err := s.MessageReactionRemove(m.ChannelID, m.MessageID, e.FormatForReaction(), m.UserID)
					if err != nil {
						log.Println(err)
					}
					break
				}
			}
			if !idMatched {
				//log.Println(m.Emoji.Name)
				if m.Emoji.Name == "❌" {
					log.Printf("REACTIONGAMESTART Removing player %s", m.UserID)
					guild.UserData.ClearPlayerData(m.UserID)
					err := s.MessageReactionRemove(m.ChannelID, m.MessageID, "❌", m.UserID)
					if err != nil {
						log.Println(err)
					}
					idMatched = true
				}
			}
			//make sure to update any voice changes if they occurred
			if idMatched {
				guild.handleTrackedMembers(s, 0, NoPriority)
				guild.GameStateMsg.Edit(s, gameStateResponse(guild))
			}

		}
	}

}

func (guild *GuildState) handleReactionPrivateUserMessage(s *discordgo.Session, m *discordgo.MessageReactionAdd) {


	//TODO: Add code here to handle reactions in private chat

	g, err := s.State.Guild(guild.PersistentGuildData.GuildID)
	if err != nil {
		log.Println(err)
	}

	if !guild.PrivateStateMsg.Exists() || !guild.PrivateStateMsg.IsReactionTo(m){
		return
	}

	log.Printf("Registered reaction in private user message!");


	//log.Print("Printing printedUsers...");
	//for _, printed := range guild.PrivateStateMsg.printedUsers {
		//log.Print("printed: " + printed);
	//}
	//log.Print("END Printing printedUsers...");


	//if (len(guild.PrivateStateMsg.printedUsers) == len(guild.PrivateStateMsg.idUsernameMap)) {
	//	return;
	//}

	var first = len(guild.PrivateStateMsg.printedUsers) == 1;

	var idUsernameMap = guild.PrivateStateMsg.idUsernameMap;
	var printedUsers = guild.PrivateStateMsg.printedUsers;

	var userId string;

	if (first) {
		userId = guild.PrivateStateMsg.printedUsers[0];
	} else {

		var message, _ = s.ChannelMessage(m.ChannelID, m.MessageID);
		for _, em := range message.Embeds {
			for _, fi := range em.Fields {
				if fi.Name == "User ID" {
					userId = fi.Value;
				}
			}
		}
	}


	idMatched := false
	for color, e := range guild.StatusEmojis[true] {
		if e.ID == m.Emoji.ID {
			idMatched = true
			log.Printf("The react button %s has been pressed", game.GetColorStringForInt(color))
			//the user doesn't exist in our userdata cache; add them

			//log.Print("TargetUserId: " + userId);
			//log.Print("MasterControllerUserId: " + m.UserID);



			log.Print("Starting checkCacheAndUser method....")
			_, added := guild.checkCacheAndAddUser(g, s, userId)
			if !added {
				log.Println("No users found in Discord for userID " + userId)
			}
			log.Print("Finished checkCacheAndUser method!")

			log.Print("Starting getColorById method....")
			playerData := guild.AmongUsData.GetByColor(game.GetColorStringForInt(color))
			log.Print("Finished getColorById method!")
			if playerData != nil {
				log.Print("Starting UpdatePlayerData....")
				guild.UserData.UpdatePlayerData(userId, playerData)
				log.Print("Finished UpdatePlayerData!")
			} else {
				//log.Println("I couldn't find any player data for that color; is your capture linked?")
			}

			//then remove the player's reaction if we matched, or if we didn't
			err := s.MessageReactionRemove(m.ChannelID, m.MessageID, e.FormatForReaction(), m.UserID)
			if err != nil {
				log.Println(err)
			}
			break
		}
	}
	if !idMatched {
		//log.Println(m.Emoji.Name)
		if m.Emoji.Name == "❌" {
			log.Printf("REACTIONPRIVATE Skipping discord user %s", m.UserID)
		}
	}

	var previousMessage = guild.PrivateStateMsg.message;

	// Not delete. Edit instead
	//s.ChannelMessageDelete(previousMessage.ChannelID, previousMessage.ID) // Deletes Message

	if (len(guild.PrivateStateMsg.printedUsers) == len(guild.PrivateStateMsg.idUsernameMap)) {
		log.Print("RETURNING!")
		s.ChannelMessageDelete(previousMessage.ChannelID, previousMessage.ID)
		return;
	}


	userId = "";
	var userName string;
	for uId, uName := range idUsernameMap {


		//log.Print("Showing printedUsers again...")
		var shownUsername = false;
		for _, username := range printedUsers {
			//log.Print("user: " + username);
			if username == uId {
				shownUsername = true;
			}
		}

		log.Print("uID:" + uId)

		if (!shownUsername) {
			log.Print("Haven't shown this user: " + uName);
			log.Print("users id: " + uId);
			userId = uId;
			userName = uName;
			//username = uName;
			break;
		}
	}


	var newEmbed = guild.PrivateStateMsg.privateMapResponse(userId, userName);


	//var newMessage = guild.PrivateStateMsg.CreateMessage(s, , guild.PrivateStateMsg.privateChannelID)

	var editedMessage = editMessageEmbed(s, previousMessage.ChannelID, previousMessage.ID, newEmbed); // Should update the message in private


	guild.PrivateStateMsg.printedUsers = append(guild.PrivateStateMsg.printedUsers, userId);


	guild.PrivateStateMsg.message = editedMessage;


	//log.Print("printing reactions:")

	for _, e := range guild.StatusEmojis[true] {
		guild.PrivateStateMsg.AddReaction(s, e.FormatForReaction())
		//log.Print("Printed reaction...")
	}
	guild.PrivateStateMsg.AddReaction(s, "❌")
	//log.Print("Reactions printed")
	//
	//log.Print("Message id: " + guild.PrivateStateMsg.message.ID);
	//log.Print("Content: " + guild.PrivateStateMsg.message.Content);


	//Create extra message here?
	log.Print("PRINTED")
}

// ToString returns a simple string representation of the current state of the guild
func (guild *GuildState) ToString() string {
	return fmt.Sprintf("%v", guild)
}

func (guild *GuildState) clearGameTracking(s *discordgo.Session) {
	//clear the discord user links to underlying player data
	guild.UserData.ClearAllPlayerData()

	//clears the base-level player data in memory
	guild.AmongUsData.ClearAllPlayerData()

	//reset all the tracking channels
	guild.Tracking.Reset()

	guild.GameStateMsg.Delete(s)
}
