package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"chatchain/internal/agents"
	"chatchain/internal/markdown"
	"chatchain/internal/ui"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"
)

// Run is the interactive chat loop, rendered through the bubbletea facade
// (internal/ui): a synchronous turn engine over ReadInput with type-ahead
// queued inside the ui (docs/design/ui-architecture.md).

// SessionFactory mints the session writer on demand: an ephemeral session
// (--no-save / no_save) carries one so /save can start persisting mid-chat.
type SessionFactory func() (*SessionWriter, error)

// Run is the interactive chat loop's entry point.
//
// Invariant: after ui.New() nothing may write to the terminal except through
// the facade — no spinner, no raw OSC/ANSI escapes, no direct stdout.
func Run(p, titleP provider.Provider, systemPrompt string, systemInteractive bool, importedHistory []provider.Message, dispatch tool.Dispatcher, mgr *mcpmgr.Manager, sw *SessionWriter, newSession SessionFactory, interact *Interactor, contextWindow int, agent AgentOptions, reqLog *RequestLog) error {
	// ---- pre-Program phase: plain stdout, the Program hasn't claimed the
	// terminal yet. The OSC background query MUST happen here (during the
	// Program it would race the event loop's stdin ownership).
	detectCodeTheme()

	var overlay *agents.Overlay
	if agent.Enabled {
		cwd, cerr := os.Getwd()
		if cerr != nil || cwd == "" {
			cwd = agent.Root
		}
		overlay = agents.NewOverlay(agent.Root, cwd)
	}
	// Token accounting (status meter, /compact, auto-compaction, the /model
	// Context tab) only exists for providers that report usage — a dedicated
	// image provider has no tokens to count. Image providers conversely get
	// the /edit command (and bill per call: no automatic retries).
	_, tokenAware := p.(provider.UsageReporter)
	_, imageProvider := p.(provider.ImageGenTunable)
	setActiveCommands(agent.Enabled, newSession != nil, tokenAware, imageProvider)
	sessionScope := ""
	if agent.Enabled {
		sessionScope = agent.Root
	}

	// Tools the user approved with "allow for this session" — consulted by the
	// approval gate before every call of an approval-requiring tool.
	approved := make(map[string]bool)

	var history []provider.Message
	persisted := 0
	if len(importedHistory) > 0 {
		history = importedHistory
		persisted = len(history)
	} else if systemPrompt != "" {
		history = append(history, provider.Message{Role: "system", Content: systemPrompt})
	}

	budget := newContextBudget(contextWindow)
	if len(history) > 0 {
		budget.update(p, history)
	}
	// Don't stamp a context-window into the meta of sessions whose provider
	// has no token accounting at all.
	if tokenAware && sw != nil && !sw.created {
		sw.SetContextWindow(budget.window)
	}

	DimStyle.Println("Chat started. Press Ctrl+C to exit.")
	saveHint := ""
	if newSession != nil {
		saveHint = ", /save"
	}
	compactHint := ", /compact"
	if !tokenAware {
		compactHint = ""
	}
	editHint := ""
	if imageProvider {
		editHint = ", /edit, /redo"
	}
	DimStyle.Println("Commands: /file [path]" + editHint + ", /session, /model" + compactHint + ", /export, /status, /tools, /debug" + saveHint + agentCommandHint(overlay))
	if id := sw.ID(); id != "" {
		DimStyle.Printf("Session: %s\n", id)
	} else if newSession != nil {
		DimStyle.Println("Session: not saved — /save [title] keeps this chat")
	}
	if n := overlay.FileCount(); n > 0 {
		DimStyle.Printf("Agent mode: AGENTS.md loaded (%d files, %.1f KB)\n", n, float64(overlay.ChainSize())/1024)
	}
	if n := overlay.SkillCount(); n > 0 {
		DimStyle.Printf("Agent mode: %d skill(s) available\n", n)
	}
	for _, warn := range overlay.Warnings() {
		DimStyle.Printf("⚠ %s\n", warn)
	}
	echoed := false
	if len(importedHistory) > 0 {
		if msgs := lastRounds(history, resumeEchoRounds); len(msgs) > 0 {
			fmt.Println()
			echoRounds(os.Stdout, msgs, sw.imagesPath())
			echoed = true
		}
	}
	if !echoed {
		fmt.Println() // one blank between the banner and the frame; a resume
		// echo already ends with its round separator
	}

	// ---- Program phase: the facade owns the terminal from here on.
	// Mirror v1's title hygiene: push the terminal's title stack now (plain
	// stdout, pre-Program) and pop it after the Program has released the
	// terminal, so the original tab title is restored on exit.
	os.Stdout.WriteString("\033[22;0t")
	defer os.Stdout.WriteString("\033[23;0t")
	u := ui.New()
	defer u.Close()
	if interact != nil {
		interact.bind(u)
	}
	defer func() { sw.Close() }() // sw may be swapped by /session
	u.SetTitle(windowTitle(sw.Title()))
	u.SetSlashCommands(activeSlashCommands)

	pushStatus := func() {
		sd := ui.StatusData{Model: statusModelLabel(p.Model(), p.Type())}
		if tokenAware {
			sd.CtxUsed, sd.CtxWindow, sd.Estimated = budget.used, budget.window, !budget.haveUsage
		}
		u.SetStatus(sd)
	}
	pushStatus()

	// The transcript is the ONLY writer to the chat area from here on: every
	// block (input, thinking, content, tool calls, notices, echoes) declares
	// itself and the transcript alone spaces them (transcript.go).
	tr := newTranscript(u, budget.counter)

	if mgr != nil {
		go reportMCPFailures(mgr, tr, u)
	}

	ctx := context.Background()
	tp, isToolProvider := p.(provider.ToolProvider)
	var tools []provider.ToolDef
	if dispatch != nil {
		tools = dispatch.Tools()
	}
	var pendingAttachments []provider.Attachment

	printErr := func(format string, a ...any) { tr.error(format, a...) }
	printDim := func(format string, a ...any) { tr.notice(format, a...) }

	var titleWG sync.WaitGroup
	titler := newSessionTitle(func() *SessionWriter { return sw }, func(s string) { u.SetTitle(windowTitle(s)) }, len(importedHistory) > 0)

	persistTurn := func() {
		if sw != nil && persisted < len(history) {
			if err := sw.AppendMessages(history[persisted:]); err != nil {
				printErr("Warning: failed to save session: %v", err)
				return
			}
			persisted = len(history)
		}
	}
	// titleNow names the session the moment its first user message exists:
	// the prompt-derived placeholder synchronously, then a model-written
	// summary of that same message as an async pass (title.go owns the state
	// machine). The pass rides its own provider instance — the turn is still
	// streaming on p, whose per-call state (usage, image results) is not
	// safe for a concurrent request. titleP is nil for dedicated image
	// providers (asked for a title they would paint one), so their
	// placeholder simply stands as the name.
	titleNow := func() {
		firstUser, gen, ok := titler.seed(history)
		if !ok || titleP == nil {
			return
		}
		titleP.SetModel(p.Model())
		titleWG.Add(1)
		go func() {
			defer titleWG.Done()
			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			titler.land(gen, generateTitleText(tctx, titleP, firstUser))
		}()
	}

	compactDeclined := 0
	compactNow := func(hint string, manual bool) {
		stop := u.Busy("Compacting context…")
		newHist, summary, retainTail, changed, cerr := compactHistory(ctx, p, history, hint)
		stop()
		if cerr != nil {
			printErr("Compaction failed: %v", cerr)
			return
		}
		if !changed {
			if manual {
				printDim("Nothing to compact yet.")
			}
			return
		}
		history = newHist
		if err := sw.AppendCompaction(summary, retainTail); err != nil {
			printErr("Warning: failed to persist compaction marker: %v", err)
		}
		persisted = len(history)
		budget.reseed(history)
		compactDeclined = 0
		printDim("Context compacted → %s", budget.status())
		pushStatus()
	}

	interruptTurn := func(watermark int, partial, partialReasoning string) {
		var persist bool
		var dropped []provider.Attachment
		history, persist, dropped = finalizeInterrupt(history, watermark, partial, partialReasoning)
		tr.notice("Interrupted.")
		// A discarded turn takes its attachments with it — hand them back so
		// the next message still carries the /edit canvas or the uploaded file
		// (cancelling a send must not silently strip them).
		if len(dropped) > 0 {
			pendingAttachments = append(append([]provider.Attachment{}, dropped...), pendingAttachments...)
			printDim("%d attachment(s) kept for your next message.", len(dropped))
		}
		if persist {
			persistTurn()
		} else {
			titler.unseed(history)
		}
		budget.update(p, history)
		pushStatus()
	}

	// retry re-attempts transient failures with linear backoff. The current
	// error's classification rides the busy status row ("Rate limited (429) —
	// retrying …"); nothing lands in the transcript until the turn gives up —
	// the caller prints the FINAL error once, instead of one red wall per
	// attempt.
	retry := func(fn func() error) error {
		err := fn()
		if err == nil || !isRetryable(err) {
			return err
		}
		// Dedicated image providers bill per attempt (a relay 5xx can even
		// arrive AFTER the upstream generation completed and charged): never
		// auto-retry — surface the error and let the user decide to respend.
		if imageProvider {
			return err
		}
		for attempt := 1; attempt <= maxRetries; attempt++ {
			stop := u.Busy(fmt.Sprintf("%s — retrying (attempt %d/%d)", describeError(err).Headline, attempt, maxRetries))
			time.Sleep(time.Duration(attempt) * time.Second)
			stop()
			err = fn()
			if err == nil || !isRetryable(err) {
				return err
			}
		}
		return err
	}

	// ---- pre-loop interactions (formerly pre-REPL on the old stack): the -S
	// system-prompt read and the startup model pick run inside the Program.
	if systemInteractive && len(importedHistory) == 0 {
		printDim("Enter a system prompt (Enter to skip):")
		if in, rerr := u.ReadInput(ctx); rerr == nil {
			if sp := strings.TrimSpace(in.Text); sp != "" {
				printDim("System prompt: %s", truncateRunes(sp, 80))
				if len(history) > 0 && history[0].Role == "system" {
					history[0].Content = sp
				} else {
					history = append([]provider.Message{{Role: "system", Content: sp}}, history...)
				}
				budget.update(p, history)
				pushStatus()
			}
		}
	}
	if p.Model() == "" {
		// v1 offered the pick at startup; ESC defers — the first message
		// re-prompts lazily (ensureModel).
		ensureModel(ctx, u, tr, p, sw)
		pushStatus()
	}

	for {
		in, rerr := u.ReadInput(ctx)
		if rerr != nil { // ErrInterrupted (idle Ctrl+C/D), ErrClosed, ctx
			titleWG.Wait()
			return nil
		}
		input := strings.TrimSpace(in.Text)
		if input == "" {
			continue
		}
		if !isReadOnlyViewer(input) {
			titleWG.Wait()
		}

		// ---- commands (minimal v2 surfaces; fidelity lands in P3) ----
		if input == "/file" || strings.HasPrefix(input, "/file ") {
			path := strings.TrimSpace(strings.TrimPrefix(input, "/file"))
			if path != "" {
				att, aerr := ReadAttachment(path)
				if aerr != nil {
					printErr("Error: %v", aerr)
				} else {
					pendingAttachments = append(pendingAttachments, att)
					printDim("Attached: %s (%s, %d bytes)", att.Filename, att.MimeType, len(att.Data))
				}
				continue
			}
			rows := make([]string, len(pendingAttachments))
			for i, a := range pendingAttachments {
				rows[i] = attachmentLabel(a)
			}
			cwd, cerr := os.Getwd()
			if cerr != nil {
				cwd, _ = os.UserHomeDir()
			}
			r, serr := u.Tabbed(ctx, ui.TabbedSpec{Panels: []ui.Panel{
				{Title: "Attached", Kind: ui.PanelMulti, Items: rows, Search: true},
				{Title: "Add", Kind: ui.PanelBrowser, Dir: cwd, Search: true},
			}})
			if serr != nil || r.Cancelled {
				continue
			}
			switch r.Focused {
			case 0: // remove the checked attachments
				if len(r.Panels[0].Checked) == 0 {
					continue
				}
				remove := map[int]bool{}
				for _, i := range r.Panels[0].Checked {
					remove[i] = true
				}
				var kept []provider.Attachment
				for i, a := range pendingAttachments {
					if !remove[i] {
						kept = append(kept, a)
					}
				}
				printDim("Removed %d attachment(s).", len(pendingAttachments)-len(kept))
				pendingAttachments = kept
			case 1: // attach the browsed file
				fp := r.Panels[1].Path
				if fp == "" {
					continue
				}
				att, aerr := ReadAttachment(fp)
				if aerr != nil {
					printErr("Error: %v", aerr)
					continue
				}
				pendingAttachments = append(pendingAttachments, att)
				printDim("Attached: %s (%s, %d bytes)", att.Filename, att.MimeType, len(att.Data))
			}
			continue
		}
		// /edit (image providers only): edit the LAST generated image. The
		// canvas is materialized into this message as an attachment — explicit
		// and persisted, so resume replays the exact reference — and the
		// stripped prompt falls THROUGH to the message path below (this is the
		// one command that sends instead of continuing the loop).
		if imageProvider && (input == "/edit" || strings.HasPrefix(input, "/edit ")) {
			prompt := strings.TrimSpace(strings.TrimPrefix(input, "/edit"))
			if prompt == "" {
				// Bare /edit: pick the canvas from every image this session
				// generated (preview beside the list), then return to the
				// composer for the prompt — the /file rhythm.
				choices := generatedImageChoices(history)
				if len(choices) == 0 {
					printDim("Nothing to edit yet — generate an image first.")
					continue
				}
				prev := newImagePreviewer(choices)
				r, serr := u.Tabbed(ctx, ui.TabbedSpec{Panels: []ui.Panel{{
					Title:   "Edit an image",
					Kind:    ui.PanelPicker,
					Prompt:  "Pick the image to edit, then type your prompt",
					Items:   imageChoiceLabels(choices),
					Details: imageChoiceDetails(choices, sw.imagesPath(), u.Width()),
					Preview: prev.render,
					Search:  true,
				}}})
				if serr != nil || r.Cancelled {
					continue
				}
				idx := r.Panels[0].Cursor
				if idx < 0 || idx >= len(choices) {
					continue
				}
				pendingAttachments = append(pendingAttachments, choices[idx].att)
				printDim("Editing %s — type your prompt.", choices[idx].att.Filename)
				continue
			}
			refs := lastGeneratedImages(history)
			if len(refs) == 0 {
				printDim("Nothing to edit yet — generate an image first.")
				continue
			}
			pendingAttachments = append(pendingAttachments, refs...)
			input = prompt
		}
		// /redo re-sends the LAST REQUEST — same reference images, same
		// prompt unless a new one is given. Image models roll differently
		// each call, and rewording works from the canvas that produced the
		// rejected result rather than from the result itself (which is what
		// ↑-recalling the /edit line, or another /edit, would do).
		if imageProvider && (input == "/redo" || strings.HasPrefix(input, "/redo ")) {
			last := lastUserMessage(history)
			if last == nil {
				printDim("Nothing to redo yet.")
				continue
			}
			prompt := strings.TrimSpace(strings.TrimPrefix(input, "/redo"))
			if prompt == "" {
				prompt = last.Content
			}
			if strings.TrimSpace(prompt) == "" {
				printDim("Nothing to redo yet — the last turn carried no prompt.")
				continue
			}
			pendingAttachments = append(pendingAttachments, last.Attachments...)
			printDim("Redoing: %s", truncateRunes(flattenLine(prompt), 60))
			input = prompt
		}
		if input == "/model" || strings.HasPrefix(input, "/model ") {
			fctx, fcancel := context.WithCancel(ctx)
			pop := u.PushCancelScope(fcancel)
			stop := u.Busy("Fetching available models")
			models, ferr := p.ListModels(fctx)
			stop()
			pop()
			// Read the cancellation state BEFORE fcancel — our own deferred
			// cancel would make fctx.Err() unconditionally non-nil.
			userCancelled := fctx.Err() != nil && ctx.Err() == nil
			fcancel()
			if userCancelled {
				continue // the user cancelled the fetch: abort /model quietly
			}
			manualModel := ferr != nil || len(models) == 0
			if ferr != nil {
				printErr("Fetching models failed: %v — enter a model name manually.", ferr)
			}
			modelPanel := ui.Panel{Title: "Model", Kind: ui.PanelInput,
				Text: p.Model(), Placeholder: "model name (e.g. gpt-4o)", InputWidth: 40}
			var modelValues []string
			if !manualModel {
				var modelLabels []string
				var modelIdx int
				modelValues, modelLabels, modelIdx = modelRows(p.Model(), models)
				modelPanel = ui.Panel{Title: "Model", Kind: ui.PanelList, Items: modelLabels, Cursor: modelIdx, Search: true}
			}
			// Tabs assemble by capability: only panels the provider can act on
			// appear, and the commit below reads them back by recorded index.
			tun, tunable := p.(provider.Tunable)
			imgTun, imgOK := p.(provider.ImageTunable)
			panels := []ui.Panel{modelPanel}
			ctxIdx, effortIdx, tempIdx, imgIdx := -1, -1, -1, -1
			var windows []int
			var levels []string
			if tokenAware {
				var windowLabels []string
				var windowIdx int
				windows, windowLabels, windowIdx = contextWindowRows(budget.window)
				ctxIdx = len(panels)
				panels = append(panels, ui.Panel{Title: "Context", Kind: ui.PanelList, Items: windowLabels, Cursor: windowIdx})
			}
			if tunable {
				var levelLabels []string
				var levelIdx int
				levels, levelLabels, levelIdx = effortRows(tun.Effort())
				effortIdx = len(panels)
				panels = append(panels, ui.Panel{Title: "Effort", Kind: ui.PanelList, Items: levelLabels, Cursor: levelIdx})
				maxTemp := 2.0
				if p.Type() == "anthropic" {
					maxTemp = 1.0
				}
				tempIdx = len(panels)
				panels = append(panels, ui.Panel{Title: "Temperature", Kind: ui.PanelSlider, Min: 0, Max: maxTemp, Step: 0.1, Value: tun.Temperature()})
			}
			// The image-generation switch: only for providers whose request
			// builders consult it (google modalities, responses builtin tool).
			if imgOK {
				imgIdx = len(panels)
				panels = append(panels, ui.Panel{Title: "Image", Kind: ui.PanelSwitch, On: imgTun.ImageOutput(),
					Prompt: "Request image generation (modalities / built-in tool)"})
			}
			// Generation-parameter tabs for dedicated image providers: choice
			// lists come from the provider, "default" omits the parameter.
			imgGen, imgGenOK := p.(provider.ImageGenTunable)
			aspectIdx, sizeIdx, negIdx := -1, -1, -1
			var aspectVals, sizeVals []string
			if imgGenOK {
				// Each knob appears only when the dialect has it: imagen
				// offers ratios+sizes+negative, images only sizes.
				opts := imgGen.ImageGenOptions()
				cur := imgGen.ImageGenParams()
				var rowLabels []string
				var rowIdx int
				if len(opts.AspectRatios) > 0 {
					aspectVals, rowLabels, rowIdx = choiceRows(cur.AspectRatio, opts.AspectRatios)
					aspectIdx = len(panels)
					panels = append(panels, ui.Panel{Title: "Aspect", Kind: ui.PanelList, Items: rowLabels, Cursor: rowIdx,
						Prompt: "Aspect ratio of generated images"})
				}
				if len(opts.ImageSizes) > 0 {
					sizeVals, rowLabels, rowIdx = choiceRows(cur.ImageSize, opts.ImageSizes)
					sizeIdx = len(panels)
					panels = append(panels, ui.Panel{Title: "Size", Kind: ui.PanelList, Items: rowLabels, Cursor: rowIdx,
						Prompt: "Output resolution tier"})
				}
				if opts.NegativePrompt {
					negIdx = len(panels)
					panels = append(panels, ui.Panel{Title: "Negative", Kind: ui.PanelInput, Text: cur.NegativePrompt,
						Placeholder: "what to avoid (empty = none)", InputWidth: 40,
						Prompt: "Negative prompt (backend support varies)"})
				}
			}
			// The edit wire format, where the dialect has two (OpenAI Images).
			jsonTun, jsonOK := p.(provider.ImageEditJSONTunable)
			jsonIdx := -1
			if jsonOK {
				jsonIdx = len(panels)
				panels = append(panels, ui.Panel{Title: "JSON edits", Kind: ui.PanelSwitch, On: jsonTun.JSONEdits(),
					Prompt: "Send /images/edits as JSON instead of multipart"})
			}
			// Last tab, and read-only: the knobs above keep their positions
			// (and their recorded indices) while this one just shows what the
			// chat is running under. A dedicated image provider takes no
			// system prompt — its request carries the last user text alone —
			// so the tab stays out of that surface entirely.
			if !imageProvider {
				if sysPanel, ok := systemPromptPanel(history, overlay.Content()); ok {
					panels = append(panels, sysPanel)
				}
			}
			r, serr := u.Tabbed(ctx, ui.TabbedSpec{Panels: panels})
			if serr != nil || r.Cancelled {
				continue
			}
			changed := false
			chosenModel := ""
			if manualModel {
				chosenModel = strings.TrimSpace(r.Panels[0].Text)
			} else {
				chosenModel = modelValues[r.Panels[0].Cursor]
			}
			if v := chosenModel; v != "" && v != p.Model() {
				p.SetModel(v)
				sw.SetModel(v)
				printDim("Model switched to %s", v)
				changed = true
			}
			if ctxIdx >= 0 {
				if v := windows[r.Panels[ctxIdx].Cursor]; v != budget.window {
					budget.setWindow(v)
					sw.SetContextWindow(v)
					printDim("Context window: %s", budget.status())
					changed = true
				}
			}
			if tunable {
				if v := levels[r.Panels[effortIdx].Cursor]; v != tun.Effort() {
					tun.SetEffort(v)
					sw.SetEffort(v)
					printDim("Effort: %s", effortLabel(v))
					changed = true
				}
				if v := r.Panels[tempIdx].Value; !floatPtrEqual(v, tun.Temperature()) {
					tun.SetTemperature(v)
					sw.SetTemperature(v)
					printDim("Temperature: %s", formatTemperature(v))
					changed = true
				}
			}
			if imgOK {
				if on := r.Panels[imgIdx].On; on != imgTun.ImageOutput() {
					imgTun.SetImageOutput(on)
					sw.SetImage(on)
					state := "off"
					if on {
						state = "on"
					}
					printDim("Image generation: %s", state)
					changed = true
				}
			}
			if imgGenOK {
				// Start from the current params so knobs without a tab keep
				// their values.
				g := imgGen.ImageGenParams()
				if aspectIdx >= 0 {
					g.AspectRatio = aspectVals[r.Panels[aspectIdx].Cursor]
				}
				if sizeIdx >= 0 {
					g.ImageSize = sizeVals[r.Panels[sizeIdx].Cursor]
				}
				if negIdx >= 0 {
					g.NegativePrompt = strings.TrimSpace(r.Panels[negIdx].Text)
				}
				if g != imgGen.ImageGenParams() {
					imgGen.SetImageGenParams(g)
					sw.SetImageGenParams(g)
					printDim("Image params: %s", imageGenLabel(g))
					changed = true
				}
			}
			if jsonOK {
				if on := r.Panels[jsonIdx].On; on != jsonTun.JSONEdits() {
					jsonTun.SetJSONEdits(on)
					sw.SetJSONEdits(on)
					state := "multipart"
					if on {
						state = "JSON"
					}
					printDim("Image edits sent as: %s", state)
					changed = true
				}
			}
			if !changed {
				printDim("No changes.")
			}
			pushStatus()
			continue
		}
		if input == "/session" || strings.HasPrefix(input, "/session ") {
			infos, lerr := ListSessions(sessionScope)
			if lerr != nil {
				printErr("Error: %v", lerr)
				continue
			}
			if len(infos) == 0 {
				printDim("No sessions yet.")
				continue
			}
			resumeRows := make([]string, len(infos))
			for i, s := range infos {
				resumeRows[i] = sessionLabel(s)
			}
			var deletable []SessionInfo
			for _, s := range infos {
				if s.ID != sw.ID() {
					deletable = append(deletable, s)
				}
			}
			deleteRows := make([]string, len(deletable))
			for i, s := range deletable {
				deleteRows[i] = sessionLabel(s)
			}
			r, serr := u.Tabbed(ctx, ui.TabbedSpec{Panels: []ui.Panel{
				{Title: "Resume", Kind: ui.PanelList, Items: resumeRows, Search: true},
				{Title: "Delete", Kind: ui.PanelMulti, Items: deleteRows, Search: true},
			}})
			if serr != nil || r.Cancelled {
				continue
			}
			if r.Focused == 1 { // delete the checked sessions
				deleted := 0
				for _, i := range r.Panels[1].Checked {
					if derr := DeleteSession(deletable[i].ID); derr != nil {
						printErr("Failed to delete %s: %v", deletable[i].ID, derr)
					} else {
						deleted++
					}
				}
				if deleted > 0 {
					printDim("Deleted %d session(s).", deleted)
				}
				continue
			}
			id := infos[r.Panels[0].Cursor].ID
			if id == sw.ID() {
				printDim("Already in this session.")
				continue
			}
			newSW, sess, lerr2 := ResumeSession(id, p)
			if lerr2 != nil {
				printErr("Error: %v", lerr2)
				continue
			}
			sw.Close()
			sw = newSW
			u.SetTitle(windowTitle(sw.Title()))
			history = sess.Messages
			persisted = len(history)
			budget.reseed(history)
			if ur, ok := p.(interface{ ResetUsage() }); ok {
				ur.ResetUsage()
			}
			pendingAttachments = nil
			titler.adopt() // the resumed bundle brings its own name
			if sess.Meta.Provider == p.Type() && sess.Meta.Model != "" {
				p.SetModel(sess.Meta.Model)
			}
			ApplySessionTuning(sess, p, false, false, budget.setWindow)
			printDim("Resumed session %s (%d messages)", id, len(history))
			if msgs := lastRounds(history, resumeEchoRounds); len(msgs) > 0 {
				var buf strings.Builder
				echoRounds(&buf, msgs, sw.imagesPath())
				tr.echo(strings.Split(strings.TrimRight(buf.String(), "\n"), "\n"))
			}
			pushStatus()
			continue
		}
		// /compact exists only for token-accounting providers — inactive
		// conditional commands fall through as plain text, like /save //skills.
		if tokenAware && (input == "/compact" || strings.HasPrefix(input, "/compact ")) {
			hint := strings.TrimSpace(strings.TrimPrefix(input, "/compact"))
			compactNow(hint, true)
			continue
		}
		if input == "/export" || strings.HasPrefix(input, "/export ") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/export"))
			if arg == "" {
				r, serr := u.Select(ctx, ui.SelectSpec{Title: "Export format", Items: []string{"HTML", "Markdown"}})
				if serr != nil || r.Cancelled {
					continue
				}
				format := exportHTML
				if r.Index == 1 {
					format = exportMarkdown
				}
				arg = exportFileName(sw.Title(), sw.ID(), format, time.Now())
			}
			lc := &lineCommitter{commit: tr.noticeLines}
			exportChat(lc, arg, sw, history, p)
			lc.flush()
			continue
		}
		if newSession != nil && (input == "/save" || strings.HasPrefix(input, "/save ")) {
			if sw != nil {
				tr.notice("Session already saving (%s).", sw.ID())
				continue
			}
			newSW, serr := newSession()
			if serr != nil {
				tr.error("Save failed: %v", serr)
				continue
			}
			sw = newSW
			sw.SetContextWindow(budget.window)
			if tun, ok := p.(provider.Tunable); ok {
				if e := tun.Effort(); e != "" {
					sw.SetEffort(e)
				}
			}
			persistTurn() // the whole backlog: persisted has stayed 0
			if name := titleFrom(strings.TrimPrefix(input, "/save"), 80); name != "" {
				titler.adoptName(name) // a user-chosen title is never overwritten
			} else {
				titleNow() // placeholder + async LLM title, as at first send
			}
			tr.notice("Session saved: %s — auto-saving from now on.", sw.ID())
			continue
		}
		if input == "/tools" || strings.HasPrefix(input, "/tools ") {
			_, _ = u.Tabbed(ctx, ui.TabbedSpec{
				RefreshEvery: 500,
				Panels: []ui.Panel{
					{Title: "Tools", Kind: ui.PanelView, Lines: toolStatusLines(dispatch, mgr),
						Refresh: func() []string { return toolStatusLines(dispatch, mgr) }},
					{Title: "MCP", Kind: ui.PanelView, Wrap: true, Lines: mcpStatusLines(mgr),
						Refresh: func() []string { return mcpStatusLines(mgr) }},
				},
			})
			continue
		}
		if input == "/debug" || strings.HasPrefix(input, "/debug ") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/debug"))
			switch arg {
			case "on":
				reqLog.SetVerbose(true)
				printDim("Request recording ON")
				continue
			case "off":
				reqLog.SetVerbose(false)
				printDim("Request recording OFF")
				continue
			}
			for {
				r, serr := u.Tabbed(ctx, ui.TabbedSpec{
					RefreshEvery: 500,
					Panels: []ui.Panel{
						{Title: "Messages", Kind: ui.PanelList, Items: requestRows(reqLog.Entries()), Search: true,
							Refresh: func() []string { return requestRows(reqLog.Entries()) }},
						{Title: "Verbose", Kind: ui.PanelSwitch, On: reqLog.Verbose()},
					},
				})
				if serr != nil || r.Cancelled {
					break
				}
				// Enter commits ALL tabs: the Verbose switch applies wherever
				// focus was (flip on the switch tab, Tab back, drill in — the
				// flip still lands).
				if v := r.Panels[1].On; v != reqLog.Verbose() {
					reqLog.SetVerbose(v)
					printDim("Request recording %s", map[bool]string{true: "ON", false: "OFF"}[v])
				}
				if r.Focused == 1 {
					break // nothing to drill into from the switch tab
				}
				entries := reqLog.Entries()
				i := r.Panels[0].Cursor
				if i < 0 || i >= len(entries) {
					break
				}
				// Drill into the entry, then reopen the list (v1 loop shape).
				_, _ = u.Tabbed(ctx, ui.TabbedSpec{Panels: []ui.Panel{
					{Title: "↑ Request", Kind: ui.PanelView, Wrap: true, Lines: requestDetailLines(entries[i])},
					{Title: "↓ Response", Kind: ui.PanelView, Wrap: true, Lines: responseDetailLines(entries[i])},
				}})
			}
			continue
		}
		if overlay != nil && (input == "/skills" || strings.HasPrefix(input, "/skills ")) {
			overlay.Refresh()
			_ = u.View(ctx, ui.ViewSpec{Title: "Skills", Lines: skillsStatusLines(overlay.Skills(), overlay.Warnings(), agent.Root)})
			continue
		}
		if input == "/status" || strings.HasPrefix(input, "/status ") {
			items := statusLines(p, budget, history, len(pendingAttachments), dispatch, mgr, sw)
			lines := make([]string, len(items))
			for i, it := range items {
				lines[i] = fmt.Sprintf("%s  %s", BoldStyle.Sprintf("%-12s", it.Name), it.Value)
			}
			_ = u.View(ctx, ui.ViewSpec{Title: "Status", Lines: lines})
			continue
		}

		// ---- message path ----
		tr.user(in.Display)

		if p.Model() == "" && !ensureModel(ctx, u, tr, p, sw) {
			continue
		}

		agentsChanged, skillsChanged := overlay.Refresh()
		if agentsChanged {
			printDim("AGENTS.md reloaded (%d files)", overlay.FileCount())
		}
		if skillsChanged {
			printDim("Skills reloaded (%d skill(s))", overlay.SkillCount())
			for _, warn := range overlay.Warnings() {
				printDim("⚠ %s", warn)
			}
		}
		sendOverlay := overlay.Content()

		extra := budget.counter.count(input)
		for _, att := range pendingAttachments {
			extra += len(att.Data) / 1000
		}
		if tokenAware && budget.shouldOfferCompact(extra, compactDeclined) {
			ok, cerr := u.Confirm(ctx, fmt.Sprintf("Context %s — compact before sending?", budget.status()), "Compact now", "Not now")
			if cerr == nil && ok {
				compactNow("", false)
			} else {
				compactDeclined = budget.used + extra
			}
		}

		history = append(history, provider.Message{Role: "user", Content: input, Attachments: pendingAttachments})
		pendingAttachments = nil
		hist0 := len(history)
		titleNow() // name the session now — the model pass races the turn

		if dispatch != nil {
			tools = dispatch.Tools()
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		// The transport reports upload progress / headers-received through
		// this slot (progress.go); the stream loops attach the handlers.
		turnCtx = withTurnProgress(turnCtx)
		sink := u.StartStream(cancelTurn)
		var reply, thinking string
		retryErr := retry(func() error {
			history = history[:hist0] // tool rounds append; reset per attempt
			var err error
			if isToolProvider && len(tools) > 0 {
				reply, thinking, err = toolLoop(turnCtx, u, sink, tr, tp, dispatch, &history, tools, sendOverlay, approved, sw.ImagesDir)
			} else {
				reply, thinking, err = streamTurn(turnCtx, u, sink, tr, p, func(w io.Writer, r io.WriteCloser) (string, string, error) {
					return p.StreamChat(turnCtx, agents.ComposeSendHistory(history, sendOverlay), w, r)
				})
			}
			return err
		})
		sink.Done()    // closes any leaked preview + pops the turn scope
		tr.resetTurn() // a dropped widget's separator is reclaimed by the next block
		cancelTurn()

		if errors.Is(retryErr, errInterrupted) {
			interruptTurn(hist0-1, reply, thinking)
			continue
		}
		if retryErr != nil {
			report := describeError(retryErr)
			tr.errorBlock(report.Headline, report.lines()...)
			history = history[:hist0-1]
			titler.unseed(history)
			continue
		}

		amsg := provider.Message{Role: "assistant", Content: reply, Reasoning: thinking}
		collectImages(p, tr, u.Width, sw.ImagesDir, &amsg)
		history = append(history, amsg)
		persistTurn()
		budget.update(p, history)
		pushStatus()
	}
}

// ensureModel is the lazy model picker for the v2 path.
func ensureModel(ctx context.Context, u *ui.UI, tr *transcript, p provider.Provider, sw *SessionWriter) bool {
	// The fetch runs under its own cancel scope: ESC aborts it (and the
	// picker) immediately instead of leaving the user hostage to the HTTP
	// timeout with an unresponsive spinner.
	fctx, fcancel := context.WithCancel(ctx)
	pop := u.PushCancelScope(fcancel)
	stop := u.Busy("Fetching available models")
	models, err := p.ListModels(fctx)
	stop()
	pop()
	userCancelled := fctx.Err() != nil && ctx.Err() == nil
	fcancel()
	if userCancelled {
		return false // user-cancelled: leave the picker entirely
	}
	var name string
	switch {
	case err != nil || len(models) == 0:
		// Listing unavailable (no such API, or it errored): fall back to a
		// manual model-name input.
		if err != nil {
			tr.error("Fetching models failed: %v", err)
		}
		r, serr := u.Tabbed(ctx, ui.TabbedSpec{Panels: []ui.Panel{{
			Title: "Model", Kind: ui.PanelInput,
			Placeholder: "model name (e.g. gpt-4o)", InputWidth: 40,
		}}})
		if serr != nil || r.Cancelled {
			return false
		}
		name = strings.TrimSpace(r.Panels[0].Text)
	default:
		r, serr := u.Select(ctx, ui.SelectSpec{Title: "Select a model", Items: models})
		if serr != nil || r.Cancelled {
			return false
		}
		name = models[r.Index]
	}
	if name == "" {
		return false
	}
	p.SetModel(name)
	sw.SetModel(name)
	tr.notice("Using model: %s", name)
	return true
}

// reportMCPFailures relays background MCP connect failures into scrollback,
// stopping when the Program exits.
func reportMCPFailures(mgr *mcpmgr.Manager, tr *transcript, u *ui.UI) {
	events := mgr.Events()
	for {
		select {
		case s, ok := <-events:
			if !ok {
				return
			}
			if s.Connected {
				continue
			}
			tr.error("⚠ MCP %s failed: %s", s.Name, strings.SplitN(s.Err, "\n", 2)[0])
		case <-u.Done():
			return
		}
	}
}

// turnPhases owns the status line's busy widget across a streaming call's
// phases (sending → waiting → thinking → composing tool call): one
// controller, so concurrent reporters (the transport callback, the tool-call
// observer) and the stream loop never fight over stop functions. Setting the
// current label again is a no-op; end() is idempotent.
type turnPhases struct {
	mu   sync.Mutex
	u    *ui.UI
	cur  string
	stop func()
}

func newTurnPhases(u *ui.UI) *turnPhases { return &turnPhases{u: u} }

func (t *turnPhases) set(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cur == label {
		return
	}
	if t.stop != nil {
		t.stop()
	}
	t.cur = label
	t.stop = t.u.Busy(label)
}

func (t *turnPhases) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stop != nil {
		t.stop()
		t.stop, t.cur = nil, ""
	}
}

// watchPhases installs the turn's phase reporting: the transport's upload /
// headers-received callbacks (progress.go) and the initial waiting label.
// Returns a cleanup detaching the handlers.
func watchPhases(ctx context.Context, phases *turnPhases) func() {
	rep := turnProgressFrom(ctx)
	rep.setHandlers(
		func(done, total int64) {
			if total >= sendProgressMinBytes {
				phases.set("Sending request")
				phases.u.BusyDetail(fmt.Sprintf("%s / %s", formatByteSize(done), formatByteSize(total)))
			}
		},
		func() { phases.set("Waiting for the model") },
	)
	phases.set("Waiting for the model")
	return func() { rep.setHandlers(nil, nil) }
}

func formatByteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// composingLabel renders the streaming header exactly like the final
// committed tool-call header ("[name …]", CodeStyle), so the widget settles
// into the collapsed form without changing its look.
func composingLabel(name string) string {
	n := "tool"
	if name != "" {
		n = displayToolName(name)
	}
	return CodeStyle.Sprint("[" + n + " …]")
}

// watchToolComposing raises the tool-call lifecycle widget ("⠋ [name …]" over
// a live "⎿ elapsed · ESC" row) while the model streams a call's arguments —
// otherwise a large write_file call is dead air. Argument text is not staged;
// the widget is the whole display. toolLoop later expands the header to its
// full form (the clock keeps running) and the result commit settles it. The
// returned cleanup detaches the observer; the widget deliberately persists.
func watchToolComposing(phases *turnPhases, tr *transcript, tp provider.ToolProvider, onFirst func()) func() {
	obs, ok := tp.(provider.ToolCallStreamObserver)
	if !ok {
		return func() {}
	}
	var mu sync.Mutex
	var last string
	obs.SetToolCallObserver(func(name, delta string) {
		if delta == "" {
			return // atomic backends (google): toolLoop raises the widget
		}
		mu.Lock()
		defer mu.Unlock()
		if last == "" && onFirst != nil {
			onFirst() // first delta: the caller ends the content stream early
		}
		label := composingLabel(name)
		if label == last {
			return
		}
		last = label
		phases.end() // the widget takes over from the status spinner
		tr.openCall(label)
	})
	return func() { obs.SetToolCallObserver(nil) }
}

// streamTurn renders one provider stream through the ui sink: busy spinner
// until the first token, the reasoning rolling window, then markdown-rendered
// content committed line by line. call runs the provider against the given
// content/reasoning writers and returns its final (reply, thinking, err).
// A ctx cancel (ESC/Ctrl+C via the ui scope) maps to errInterrupted with the
// partials the user actually saw — same contract as streamResponse.
func streamTurn(ctx context.Context, u *ui.UI, sink ui.StreamSink, tr *transcript, p any, call func(w io.Writer, r io.WriteCloser) (string, string, error)) (string, string, error) {
	reasonPr, reasonPw := io.Pipe()
	contentPr, contentPw := io.Pipe()
	var reply, thinking string
	var streamErr error
	var contentBuf, reasonBuf strings.Builder
	done := make(chan struct{})

	// Progressive frames from a dedicated image provider: this turn runs no
	// tool, so the first frame raises the widget that hosts it (the final
	// tr.image morphs that same widget into the picture).
	defer watchImagePartials(u, p, func() { tr.openCall("image") })()

	go func() {
		defer close(done)
		defer contentPw.Close()
		defer reasonPw.Close()
		reply, thinking, streamErr = call(
			io.MultiWriter(contentPw, &contentBuf),
			teeWriteCloser{io.MultiWriter(reasonPw, &reasonBuf), reasonPw})
	}()

	interrupted := func() bool { return ctx.Err() != nil }
	fail := func(err error) (string, string, error) {
		if interrupted() {
			return contentBuf.String(), reasonBuf.String(), errInterrupted
		}
		return "", "", err
	}

	firstChunk := make([]byte, 4096)
	var firstN int
	var readErr error
	hasReasoning := false

	// Status-line phases cover the pre-output window (sending/waiting); once
	// anything renders, the transcript's widgets and blocks take over.
	phases := newTurnPhases(u)
	defer phases.end()
	defer watchPhases(ctx, phases)()

	firstN, readErr = reasonPr.Read(firstChunk)
	if readErr != nil {
		readErr = nil
		firstN, readErr = contentPr.Read(firstChunk)
	} else {
		hasReasoning = true
	}

	if readErr != nil {
		<-done
		if streamErr != nil {
			return fail(streamErr)
		}
		if hasImages(p) { // image-only response: no text is a valid turn
			return reply, thinking, nil
		}
		return fail(readErr)
	}

	newContent := func() (*markdown.Writer, *uiMDSink) {
		msink := newUIMDSink(sink, tr.contentBlock(), u.Width)
		return markdown.NewWriter(msink), msink
	}

	if hasReasoning {
		start := time.Now()
		phases.end() // the thinking widget takes over from the status spinner
		meter := tr.openThinking()
		meter.add(string(firstChunk[:firstN]))
		io.Copy(meter, reasonPr) // count only; the tee already keeps the text
		tr.settleThinking(start)

		firstN, readErr = contentPr.Read(firstChunk)
		if readErr != nil {
			<-done
			if interrupted() || streamErr != nil {
				return fail(streamErr)
			}
			if hasImages(p) { // thought + image, no text: a valid turn
				return reply, thinking, nil
			}
			// Reasoning-only response: render the reasoning as the answer.
			mdw, msink := newContent()
			mdw.Write([]byte(thinking))
			mdw.Flush()
			msink.flush()
			return thinking, thinking, nil
		}
	}

	phases.end() // streaming output is its own progress from here
	mdw, msink := newContent()
	mdw.Write(firstChunk[:firstN])
	io.Copy(mdw, contentPr)
	mdw.Flush()
	msink.flush()
	<-done

	if interrupted() || streamErr != nil {
		return fail(streamErr)
	}
	return reply, thinking, nil
}

// toolLoop is the v2 twin of executeWithTools: rounds of streaming +
// tool execution rendered through the ui frame (Busy labels instead of the
// stderr spinner; per-tool cancel scopes instead of raw-mode watches).
func toolLoop(ctx context.Context, u *ui.UI, sink ui.StreamSink, tr *transcript, tp provider.ToolProvider, dispatch tool.Dispatcher, history *[]provider.Message, tools []provider.ToolDef, overlay string, approved map[string]bool, imgDir func() string) (string, string, error) {
	// No round cap: the user is the brake (ESC cancels the turn; approval
	// gates cover mutating tools) — industry parity with the major CLIs.
	for {
		content, reasoning, toolCalls, err := streamToolRound(ctx, u, sink, tr, tp, agents.ComposeSendHistory(*history, overlay), tools)
		if err != nil {
			return content, reasoning, err
		}
		if len(toolCalls) == 0 {
			return content, reasoning, nil
		}

		msg := provider.Message{Role: "assistant", Content: content, ToolCalls: toolCalls}
		if rcp, ok := tp.(provider.RawContentProvider); ok {
			msg.RawContent = rcp.LastRawContent()
		}
		collectImages(tp, tr, u.Width, imgDir, &msg)
		*history = append(*history, msg)

		for _, tc := range toolCalls {
			header := CodeStyle.Sprint(toolCallHeader(tc))

			// The lifecycle widget: "⠋ [name args…]" over a live "⎿ elapsed ·
			// ESC" row. Composing already raised it (the header expands in
			// place, the clock keeps running); atomic backends raise it here.
			// It spins through approval and execution, and settles into the
			// committed header + result below.
			tr.openCall(header)

			// Approval gate: state-changing tools (tool.ApprovalReporter) run
			// only with the user's consent — once, or for the whole session.
			// The widget header above shows what is being approved.
			if needsApproval(dispatch, tc.Name) && !approved[tc.Name] {
				choice, aerr := u.Select(ctx, ui.SelectSpec{
					Title: fmt.Sprintf("%s wants to modify files — allow?", displayToolName(tc.Name)),
					Items: []string{"Allow once", "Allow for this session", "Deny"},
				})
				if aerr != nil {
					return "", "", aerr
				}
				if choice.Cancelled || choice.Index == 2 {
					const declined = "The user declined this call."
					tr.settleCall(header)
					lc := &lineCommitter{commit: tr.toolLines}
					printToolResult(lc, declined, true)
					lc.flush()
					*history = append(*history, provider.Message{
						Role:         "tool",
						Content:      declined,
						ToolCallID:   tc.ID,
						ToolCallName: tc.Name,
						IsError:      true,
					})
					continue
				}
				if choice.Index == 1 {
					approved[tc.Name] = true
				}
			}

			toolCtx, cancel := context.WithCancel(ctx)
			pop := u.PushCancelScope(cancel)
			resultText, isError, callErr := dispatch.CallTool(toolCtx, tc.Name, tc.Arguments)
			pop()
			cancel()
			if callErr != nil {
				resultText = fmt.Sprintf("Error calling tool: %v", callErr)
				isError = true
			}

			// Settle: the widget morphs in place into its final collapsed
			// form — the spinner disappears as the result writes back.
			tr.settleCall(header)
			lc := &lineCommitter{commit: tr.toolLines}
			printToolResult(lc, resultText, isError)
			lc.flush()

			*history = append(*history, provider.Message{
				Role:         "tool",
				Content:      resultText,
				ToolCallID:   tc.ID,
				ToolCallName: tc.Name,
				IsError:      isError,
			})

			if ctx.Err() != nil { // tool cancelled via ESC propagates as interrupt
				return "", "", errInterrupted
			}
		}
	}
}

// contentTap is the content writer handed to the provider (stream goroutine
// only): every byte lands in the history buffer; bytes flow to the render
// pipe until the observer cuts it at the first tool delta, and anything after
// the cut spills — the round renders the spill after the stream so interleaved
// trailing text stays VISIBLE (pre-deferral it rendered, misordered; silently
// dropping it would show the model text in history the user never saw). The
// first byte marks the transcript's content block open — on this goroutine,
// happens-before any tool delta that follows.
type contentTap struct {
	pw     *io.PipeWriter
	buf    *strings.Builder
	mark   func()
	marked bool
	closed bool
	spill  strings.Builder
}

func (c *contentTap) Write(p []byte) (int, error) {
	if !c.marked {
		c.marked = true
		c.mark()
	}
	c.buf.Write(p)
	if !c.closed {
		if _, err := c.pw.Write(p); err == nil {
			return len(p), nil
		}
		c.closed = true
	}
	c.spill.Write(p)
	return len(p), nil
}

// streamToolRound is the v2 twin of streamToolRound (interactive only —
// Once keeps the v1 quiet path).
func streamToolRound(ctx context.Context, u *ui.UI, sink ui.StreamSink, tr *transcript, tp provider.ToolProvider, history []provider.Message, tools []provider.ToolDef) (string, string, []provider.ToolCall, error) {
	reasonPr, reasonPw := io.Pipe()
	contentPr, contentPw := io.Pipe()
	var content, reasoning string
	var toolCalls []provider.ToolCall
	var streamErr error
	var contentBuf, reasonBuf strings.Builder
	done := make(chan struct{})

	tr.beginRound() // a failed prior round may have leaked streaming guards
	tap := &contentTap{pw: contentPw, buf: &contentBuf, mark: tr.markContent}

	// renderSpill commits content the model streamed AFTER its first tool
	// delta (the tap cut the render pipe there). It lands below the raised
	// widget — misordered like the pre-deferral behavior, but visible.
	renderSpill := func() {
		if tap.spill.Len() == 0 {
			return
		}
		msink := newUIMDSink(sink, tr.contentBlock(), u.Width)
		mdw := markdown.NewWriter(msink)
		mdw.Write([]byte(tap.spill.String()))
		mdw.Flush()
		msink.flush()
	}

	// Phase reporting and the composing observer install BEFORE the stream
	// goroutine exists: SetToolCallObserver writes an unguarded provider
	// field, so the install must happen-before the goroutine that reads it.
	// Cleanups run after <-done (every return path passes it).
	phases := newTurnPhases(u)
	defer phases.end()
	defer watchPhases(ctx, phases)()
	// On the first composing delta the content stream is over for this round
	// (models emit text before tool calls): closing the content pipe makes the
	// renderer's io.Copy return NOW, so buffered blocks (a trailing table)
	// flush and closeContent raises the deferred widget promptly — instead of
	// after the full arg stream. Later provider writes to the closed pipe are
	// dropped (fmt.Fprint discards the error; history uses the provider's own
	// accumulator).
	defer watchToolComposing(phases, tr, tp, func() { contentPw.Close() })()
	defer watchImagePartials(u, tp, nil)()

	go func() {
		defer close(done)
		defer contentPw.Close()
		defer reasonPw.Close()
		content, reasoning, toolCalls, streamErr = tp.StreamChatWithTools(ctx, history, tools,
			tap,
			teeWriteCloser{io.MultiWriter(reasonPw, &reasonBuf), reasonPw})
	}()

	interrupted := func() bool { return ctx.Err() != nil }
	fail := func(err error) (string, string, []provider.ToolCall, error) {
		if interrupted() {
			return contentBuf.String(), reasonBuf.String(), nil, errInterrupted
		}
		return "", "", nil, err
	}

	firstChunk := make([]byte, 4096)
	var firstN int
	var readErr error
	hasReasoning := false

	firstN, readErr = reasonPr.Read(firstChunk)
	if readErr != nil {
		readErr = nil
		firstN, readErr = contentPr.Read(firstChunk)
	} else {
		hasReasoning = true
	}

	if readErr != nil {
		<-done
		if interrupted() || streamErr != nil {
			return fail(streamErr)
		}
		if len(toolCalls) > 0 { // tool calls with no text
			renderSpill()
			return content, reasoning, toolCalls, nil
		}
		if hasImages(tp) { // image-only response: no text is a valid turn
			return content, reasoning, nil, nil
		}
		return fail(readErr)
	}

	newContent := func() (*markdown.Writer, *uiMDSink) {
		msink := newUIMDSink(sink, tr.openContent(), u.Width)
		return markdown.NewWriter(msink), msink
	}

	if hasReasoning {
		start := time.Now()
		phases.end() // the thinking widget takes over from the status spinner
		meter := tr.openThinking()
		meter.add(string(firstChunk[:firstN]))
		io.Copy(meter, reasonPr) // count only; the tee already keeps the text
		tr.settleThinking(start)

		firstN, readErr = contentPr.Read(firstChunk)
		if readErr != nil {
			<-done
			if interrupted() || streamErr != nil {
				return fail(streamErr)
			}
			if len(toolCalls) > 0 {
				renderSpill()
				return content, reasoning, toolCalls, nil
			}
			if hasImages(tp) { // thought + image, no text: a valid turn
				tr.closeContent()
				return content, reasoning, nil, nil
			}
			// Reasoning-only response: render the reasoning as the answer.
			mdw, msink := newContent()
			mdw.Write([]byte(reasoning))
			mdw.Flush()
			msink.flush()
			tr.closeContent()
			return reasoning, reasoning, nil, nil
		}
	}

	if firstN > 0 {
		phases.end() // streaming output is its own progress from here
		mdw, msink := newContent()
		mdw.Write(firstChunk[:firstN])
		io.Copy(mdw, contentPr)
		mdw.Flush()
		msink.flush()
		tr.closeContent()
	} else {
		io.Copy(io.Discard, contentPr)
	}
	<-done

	if interrupted() || streamErr != nil {
		return fail(streamErr)
	}
	renderSpill()
	return content, reasoning, toolCalls, nil
}

// windowTitle is the terminal-tab title for a session: its own title, or the
// app name before one exists (whitespace-only counts as none, mirroring the
// v1 terminalTitleSeq fallback).
func windowTitle(title string) string {
	if strings.TrimSpace(title) == "" {
		return appTitle
	}
	return title
}

// statusModelLabel is the status line's model field: the provider type stands
// in until a model is chosen (v1 composer-status contract).
func statusModelLabel(model, providerType string) string {
	if model != "" {
		return model
	}
	return providerType
}
