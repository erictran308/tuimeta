// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import "github.com/erictran308/tuimeta/helper/internal/proto"

// spec is one scripted message. from and the people it names count from 1
// in the chat's members; 0 is you.
type spec struct {
	from       int
	text       string
	ents       []ent
	media      []mediaSpec
	re         int // the index of the message answered; 0 for none
	reacts     []react
	edited     bool
	fwd        bool
	sameMs     bool // sent in the same millisecond as the one before
	service    string
	unsup      string
	preview    *proto.LinkPreview
	previewArt bool
}

type ent struct {
	typ  proto.EntityType
	sub  string
	url  string
	user int
}

type react struct {
	who   int
	emoji string
}

type mediaSpec struct {
	kind       proto.MediaKind
	art        art
	name, mime string
	title      string // a document's text
	duration   int
	viewOnce   bool
}

func say(from int, text string) spec { return spec{from: from, text: text} }

func event(from int, sentence string) spec { return spec{from: from, service: sentence} }

func (s spec) with(e ...ent) spec         { s.ents = append(s.ents, e...); return s }
func (s spec) reply(i int) spec           { s.re = i; return s }
func (s spec) react(r ...react) spec      { s.reacts = append(s.reacts, r...); return s }
func (s spec) edit() spec                 { s.edited = true; return s }
func (s spec) forwarded() spec            { s.fwd = true; return s }
func (s spec) sameTime() spec             { s.sameMs = true; return s }
func (s spec) attach(m ...mediaSpec) spec { s.media = append(s.media, m...); return s }

func r(who int, emoji string) react { return react{who, emoji} }

func bold(sub string) ent             { return ent{typ: proto.Bold, sub: sub} }
func italic(sub string) ent           { return ent{typ: proto.Italic, sub: sub} }
func code(sub string) ent             { return ent{typ: proto.InlineCode, sub: sub} }
func strike(sub string) ent           { return ent{typ: proto.Strike, sub: sub} }
func pre(sub string) ent              { return ent{typ: proto.Pre, sub: sub} }
func quoted(sub string) ent           { return ent{typ: proto.Quote, sub: sub} }
func link(url string) ent             { return ent{typ: proto.Link, sub: url, url: url} }
func mention(sub string, who int) ent { return ent{typ: proto.Mention, sub: sub, user: who} }

// Pictures in three shapes: tall, wide and square.
func tall(seed int) mediaSpec {
	return mediaSpec{kind: proto.Photo, art: art{photoArt, 360, 540, seed}}
}
func wide(seed int) mediaSpec {
	return mediaSpec{kind: proto.Photo, art: art{photoArt, 640, 320, seed}}
}
func square(seed int) mediaSpec {
	return mediaSpec{kind: proto.Photo, art: art{photoArt, 480, 480, seed}}
}

func viewOnce() mediaSpec { return mediaSpec{kind: proto.Photo, viewOnce: true} }

func sticker(seed int) mediaSpec {
	return mediaSpec{kind: proto.Sticker, art: art{stickerArt, 256, 256, seed}}
}

func video(name string, seconds, seed int) mediaSpec {
	return mediaSpec{kind: proto.Video, art: art{videoArt, 640, 360, seed}, name: name, duration: seconds}
}

func voice(seconds int) mediaSpec {
	return mediaSpec{kind: proto.Voice, name: "audioclip.m4a", duration: seconds}
}

func document(name, title string) mediaSpec {
	return mediaSpec{kind: proto.FileMedia, name: name, mime: "application/pdf", title: title}
}

// Messenger ---------------------------------------------------------------

// aliceChat: an end-to-end encrypted dm with most kinds of message.
func aliceChat() map[int]spec {
	return map[int]spec{
		3:  say(1, "We should go back to that lake before it gets cold 🏞️"),
		5:  say(0, "Absolutely. First weekend of next month?").react(r(1, "👍")),
		12: say(1, "Reminder: bring your passport, the charger, and the code for the gate is 4512").with(bold("Reminder:"), italic("passport"), code("4512")),
		16: say(0, "Dinner at 7 8 pm, they moved our table").with(strike("7")),
		20: say(1, "Found the cabin: https://example.com/cabins/pine-lodge?ref=chat and it has a sauna!").
			with(link("https://example.com/cabins/pine-lodge?ref=chat")).react(r(1, "❤️"), r(0, "❤️")),
		24: say(0, "Okay so here's my thinking for the weekend: we take the early train on Saturday, drop our bags at the cabin, hike up to the ridge in the afternoon while the weather holds, and keep Sunday free for the lake and a very long lunch. If it rains we can always swap the days around. Thoughts? 🤔"),
		30: say(1, "Sunset from the ridge 🌄 more tomorrow").attach(tall(1), wide(2), square(3)).react(r(0, "😍")),
		33: say(0, "These are gorgeous!!").react(r(1, "🥰")),
		41: say(1, "Here's the plan for the trip").attach(document("trip-plan.pdf", "Trip plan: Pine Lodge, 3 nights")),
		44: say(1, "What time are we meeting?"),
		45: say(0, "See you at the station at 8:15 (not 8:00!)").edit(),
		46: say(1, "Got it, 8:15 ☕"),
		47: say(0, "I'll grab coffees").reply(46),
		58: say(1, "Remember what you said about the lake? 😄").reply(3),
		59: say(1, "Also: I booked the sauna for Saturday evening 🧖"),
	}
}

// tripChat: a group of five with events, a poll, a sticker and a mention.
func tripChat() map[int]spec {
	return map[int]spec{
		0:  event(2, "Ben named the group Weekend Trip ⛺"),
		1:  event(0, "You added Dmitri to the group"),
		8:  say(3, "Who's driving? 🚗"),
		12: say(4, "I can drive on the way back, not before 10 though"),
		18: say(3, "Packing list so far: tent, two stoves, the big cooler, headlamps, the first-aid kit, and whatever snacks Ben insists are essential. Tell me if I forgot something important before Thursday night!"),
		25: event(3, "Chloé unsent a message"),
		33: spec{from: 4, unsup: "[Poll]"},
		37: spec{from: 2}.attach(sticker(0)),
		40: say(1, "🎉 @Ben can you book the car for Friday?").with(mention("@Ben", 2)),
		41: say(2, "On it").reply(40),
		44: say(0, "I'll bring the speaker 🔊").react(r(0, "🔥"), r(2, "🔥")),
		47: say(3, "Same here!"),
		48: say(4, "Same!").sameTime(),
		50: say(1, "Final headcount: 5 people, 2 cars").react(r(2, "👍"), r(4, "👍"), r(3, "😮")),
		52: say(0, "Train times: 08:15, 09:40, 11:05").forwarded(),
	}
}

// benChat: a muted dm with a photo, a video and a voice message.
func benChat() map[int]spec {
	return map[int]spec{
		10: say(1, "Check out the new bike 🚲"),
		20: spec{from: 1}.attach(wide(5)),
		29: say(1, "Want to ride on Sunday?"),
		30: say(0, "Yes! Where?").reply(29),
		50: say(1, "First ride 🎬").attach(video("IMG_2041.MP4", 14, 1)),
		55: spec{from: 1}.attach(voice(7)),
	}
}

// bookChat: a group with a quote, a code block and a long link.
func bookChat() map[int]spec {
	return map[int]spec{
		0:  event(2, "Eve named the group Book Club 📚"),
		10: say(2, "Favourite line so far:\nThe sea was not a mask. No more was the moon.").with(quoted("The sea was not a mask. No more was the moon.")),
		22: say(3, "My page notes:\nch. 3, p. 112\nch. 5, p. 40").with(pre("ch. 3, p. 112\nch. 5, p. 40")),
		31: say(2, "I finished it last night and honestly the last chapter changed how I read everything before it. The narrator was never describing the town at all, it was the letters the whole time, and now I want to start again from page one."),
		40: say(1, "Interview with the author: https://example.org/2026/09/an-unusually-long-interview-address-that-should-wrap-somewhere-in-the-middle").
			with(link("https://example.org/2026/09/an-unusually-long-interview-address-that-should-wrap-somewhere-in-the-middle")),
		59: say(0, "Next meeting Thursday at 7, my place"),
	}
}

// chloeChat: your last two messages aren't read yet.
func chloeChat() map[int]spec {
	return map[int]spec{
		15: say(1, "Ça marche, à demain ! 🥐"),
		30: say(0, "Thanks for the book, I'm halfway through").react(r(1, "❤️")),
		58: say(0, "Are you free for lunch on Wednesday?"),
		59: say(0, "Or Thursday works too"),
	}
}

// dmitriChat: archived, one unread.
func dmitriChat() map[int]spec {
	return map[int]spec{
		20: say(1, "Привет! Long time no see 👋"),
		59: say(1, "Are you coming to the reunion?"),
	}
}

// Instagram -----------------------------------------------------------------

// mayaChat: a link preview and a photo shown only once.
func mayaChat() map[int]spec {
	return map[int]spec{
		9:  say(1, "Have you climbed The Arête yet?"),
		30: say(0, "Not yet, it's on my list for this season").react(r(1, "🙌")),
		44: spec{from: 1}.attach(viewOnce()),
		52: func() spec {
			s := say(1, "This is the route I told you about: https://example.com/routes/the-arete").
				with(link("https://example.com/routes/the-arete"))
			s.preview = &proto.LinkPreview{
				URL:         "https://example.com/routes/the-arete",
				Title:       "The Arête: a classic 5.8 multi-pitch",
				Description: "Four pitches of clean granite with a famous knife-edge finish. Best climbed in the morning, before the sun reaches the face.",
			}
			s.previewArt = true
			return s
		}(),
		57: say(0, "Still haven't, but soon!").reply(9),
		59: say(1, "Did you see it? 👆"),
	}
}

// crewChat: a group of five with an album and an edit.
func crewChat() map[int]spec {
	return map[int]spec{
		0:  event(0, "You created the group"),
		2:  event(2, "Sam added Noor to the group"),
		15: say(3, "Chalk bag sale at the gym this week"),
		28: say(2, "Saturday at the crag 🧗‍♀️").attach(wide(6), tall(7), square(0)).react(r(0, "❤️"), r(1, "❤️"), r(3, "🔥")),
		33: say(4, "Who's in for Tuesday night?"),
		36: spec{from: 1, unsup: "[Poll]"},
		41: say(3, "Bringing the new rope").edit(),
		45: say(0, "Count me in 💪").reply(33),
	}
}

// oliverChat: muted, with a tall photo and a forward.
func oliverChat() map[int]spec {
	return map[int]spec{
		33: say(1, "View from the top").attach(tall(4)),
		45: say(1, "Ten tips for better sleep 😴").forwarded(),
	}
}

// dealsChat: a message request.
func dealsChat() map[int]spec {
	return map[int]spec{
		0:  say(1, "Hi! 👋 Quick question"),
		1:  say(1, "Are you interested in a collaboration?"),
		59: say(1, "Last chance: reply YES to learn more 🔥"),
	}
}

// noorChat: a photo with a caption and a sticker.
func noorChat() map[int]spec {
	return map[int]spec{
		25: say(1, "New sketch ✏️").attach(tall(2)),
		40: spec{from: 1}.attach(sticker(1)),
		48: spec{from: 0}.attach(voice(4)),
	}
}

// junChat: a voice message.
func junChat() map[int]spec {
	return map[int]spec{
		20: say(1, "Lunch tomorrow? 🍜"),
		48: spec{from: 1}.attach(voice(12)),
	}
}

// Fillers -------------------------------------------------------------------

var fillers = []string{
	"Morning! ☀️",
	"Did you see the forecast for Saturday?",
	"Haha yes 😂",
	"I'll be there around 6",
	"Running a bit late, sorry!",
	"No worries, take your time",
	"Can you send me the address again?",
	"Sure thing 👍",
	"That sounds great",
	"I was thinking we could try the new place on 5th street, the one with the big windows and the long tables. Apparently their noodles are excellent and it's never too crowded on weekdays.",
	"👀",
	"ok",
	"Let me check and get back to you",
	"Just finished work, finally 😮‍💨",
	"Where did you park last time?",
	"Good night 🌙",
	"Thanks again for yesterday, it was really fun",
	"Do we need to bring anything?",
	"Maybe some snacks 🍿",
	"I can't stop listening to that album you sent",
	"Wait, which one?",
	"The one with the blue cover 💙",
	"Here's a thought: if we leave early enough we can avoid the traffic entirely, grab coffee on the way, and still have the whole afternoon for the lake. Worst case we just come back a bit earlier than planned.",
	"Sounds like a plan",
	"🎉🎉🎉",
	"Is the meeting still on for Thursday?",
	"Yes, 3pm",
	"Perfect",
	"How was the trip?",
	"Long story, I'll tell you over dinner",
	"Coffee or tea? ☕🍵",
	"Both, obviously",
	"Can't wait!",
	"Did you get my message?",
	"Yep, sorry, was driving",
	"日本語のメッセージもテストします",
	"Weekend plans?",
	"Nothing yet, open to ideas",
	"Call you later 📞",
	"👋",
}

var strangerFillers = []string{
	"Hey! 👋",
	"Are you there?",
	"I have an offer for you",
	"Limited time only 🔥",
	"Check your DMs",
	"Just following up",
	"Reply YES to learn more",
	"Don't miss out!",
}

var fillerReactions = []string{"❤️", "😂", "👍", "😮", "🔥"}

// filler is message i when the script has nothing special there: someone
// says a line, now and then with a reaction.
func filler(c *chat, cd chatDef, ci, i int) spec {
	if cd.strangers {
		return say(1, strangerFillers[(i*3)%len(strangerFillers)])
	}
	var s spec
	s.text = fillers[(cd.fillerStart+i*7+ci*3)%len(fillers)]
	if cd.kind == proto.DM {
		if (i*7+ci*3)%5 < 2 {
			s.from = 0
		} else {
			s.from = 1
		}
	} else {
		s.from = (i*3 + ci) % (len(c.members) + 1)
	}
	if (i*13+ci)%11 == 0 {
		who := 1
		if s.from == 1 {
			who = 0
		}
		s.reacts = []react{{who, fillerReactions[(i+ci)%len(fillerReactions)]}}
	}
	return s
}
