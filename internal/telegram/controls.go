package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"
)

// Controls is what the admin chat's buttons and commands do. The app
// implements it. Each method returns the reply posted in the chat; an error
// means nothing was done (the press can be tried again).
type Controls interface {
	// Undo lifts every ban of the report's member, marks the bot's actions
	// against them overturned, and lists the groups they were removed from.
	Undo(ctx context.Context, reportID int64, by Actor) (string, error)
	// Ban acts on a watch-only report: deletes the message (only while it is
	// younger than act_on_replay_max_age), removes and bans the sender.
	Ban(ctx context.Context, reportID int64, by Actor) (string, error)
	// AddToBanList bans (and removes everywhere) a member a human admin
	// removed.
	AddToBanList(ctx context.Context, reportID int64, by Actor) (string, error)
	// Resume lifts every pause; queued actions are checked again first.
	Resume(ctx context.Context, by Actor) (string, error)
	// Pause stops every action, deletes included, until /resume.
	Pause(ctx context.Context, by Actor) (string, error)
	// Reload reads the config file again (whole, or not at all).
	Reload(ctx context.Context, by Actor) (string, error)
	// Join joins a group by invite link, only when its parent is a
	// configured community.
	Join(ctx context.Context, link string, by Actor) (string, error)
	// Status describes the bot's state.
	Status(ctx context.Context) (string, error)
}

// Actor is the admin who pressed a button or sent a command.
type Actor struct {
	ID   int64
	Name string
}

// String is how the ledger and the logs name the actor.
func (a Actor) String() string {
	return "telegram:" + strconv.FormatInt(a.ID, 10) + " (" + a.Name + ")"
}

func actorOf(u models.User) Actor {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	if name == "" {
		name = "user " + strconv.FormatInt(u.ID, 10)
	}
	return Actor{ID: u.ID, Name: name}
}
