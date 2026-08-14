package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

//
// TYPES
//

// endpoint is one screen: the calls of a single mock, in arrival order.
type endpoint struct {
	name     string
	requests []Request
	unread   int
	override int // forced status, 0 when the endpoint answers normally
}

// model is the bubbletea state of the whole TUI.
type model struct {
	server    *Server
	endpoints []*endpoint
	selected  int // index into endpoints
	cursor    int // index into the visible requests of the selected endpoint
	focus     int // which pane the arrow keys drive, paneSidebar or paneRequests
	paused    bool
	pending   []Request // held while paused, flushed on resume
	searching bool
	filter    textinput.Model
	detail    viewport.Model
	width     int
	height    int
}

//
// CONSTANTS
//

const (
	// allScreen aggregates every call, and is always the first endpoint.
	allScreen = "All"

	sidebarWidth = 30
	footerHeight = 1
)

// Both panes are vertical lists, so the arrows drive whichever one has the
// focus and the horizontal keys move the focus between them.
const (
	paneSidebar = iota
	paneRequests
)

// overrideCycle is what the `s` key walks through, 0 meaning no override.
var overrideCycle = []int{0, 500, 429, 200}

// jsonToken matches the pieces of indented JSON that get a color. Object keys
// come first so they win over plain strings.
var jsonToken = regexp.MustCompile(
	`"(?:\\.|[^"\\])*"\s*:` +
		`|"(?:\\.|[^"\\])*"` +
		`|\btrue\b|\bfalse\b|\bnull\b` +
		`|-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?`)

// Colors follow jq's defaults: keys bold blue, strings green, null grey.
var (
	keyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
	stringStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	nullStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	titleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	selectStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	unreadStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	sidebarStyle = lipgloss.NewStyle().
			Width(sidebarWidth).
			BorderStyle(lipgloss.NormalBorder()).
			BorderRight(true).
			BorderForeground(lipgloss.Color("240")).
			PaddingRight(1)
)

// keyActions maps a key to what it does, keeping the key handler flat.
var keyActions = map[string]func(m *model){
	"tab":       (*model).toggleFocus,
	"shift+tab": (*model).toggleFocus,
	"left":      (*model).focusSidebar,
	"h":         (*model).focusSidebar,
	"right":     (*model).focusRequests,
	"l":         (*model).focusRequests,
	"down":      (*model).moveDown,
	"j":         (*model).moveDown,
	"up":        (*model).moveUp,
	"k":         (*model).moveUp,
	" ":         (*model).togglePause,
	"/":         (*model).startSearch,
	"s":         (*model).cycleOverride,
	"c":         (*model).clearScreen,
	"esc":       (*model).clearFilter,
}

//
// CODE
//

// newModel builds the TUI with one screen per mock plus the All screen.
func newModel(server *Server) *model {
	filter := textinput.New()
	filter.Prompt = "/"
	filter.Placeholder = "filtrar por método, path, header ou corpo"

	built := &model{
		server:    server,
		endpoints: []*endpoint{{name: allScreen}},
		filter:    filter,
		detail:    viewport.New(0, 0),
	}

	// pre-create a screen per mock so they show up before the first call
	for _, name := range server.Endpoints() {
		built.screen(name)
	}

	// a terminal that never reports its size still gets a usable screen
	built.resize(80, 24)

	return built
}

// screen returns the endpoint named name, creating it when it is new.
func (m *model) screen(name string) *endpoint {
	for _, candidate := range m.endpoints {
		// already known: reuse it
		if candidate.name == name {
			return candidate
		}
	}

	created := &endpoint{name: name}
	m.endpoints = append(m.endpoints, created)
	return created
}

// current returns the endpoint being shown.
func (m *model) current() *endpoint {
	return m.endpoints[m.selected]
}

// visible returns the requests of the current screen after the filter.
func (m *model) visible() []Request {
	requests := m.current().requests
	needle := strings.ToLower(m.filter.Value())

	// no filter: everything is visible
	if needle == "" {
		return requests
	}

	matched := make([]Request, 0, len(requests))
	for _, request := range requests {
		// haystack misses the needle: drop the row
		if !strings.Contains(strings.ToLower(haystack(request)), needle) {
			continue
		}
		matched = append(matched, request)
	}

	return matched
}

// haystack flattens a request into the text the filter searches.
func haystack(request Request) string {
	var builder strings.Builder
	builder.WriteString(request.Method)
	builder.WriteString(request.Path)
	builder.WriteString(request.Query)
	builder.WriteString(request.Body)

	for name, values := range request.Headers {
		builder.WriteString(name)
		builder.WriteString(strings.Join(values, " "))
	}

	return builder.String()
}

func (m *model) Init() tea.Cmd {
	return textinput.Blink
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := msg.(type) {
	case Request:
		m.add(typed)
		return m, nil

	case tea.WindowSizeMsg:
		m.resize(typed.Width, typed.Height)
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(typed)
	}

	return m, nil
}

// add files a captured request into its screen and the All screen.
func (m *model) add(request Request) {
	// paused: hold it so the list does not move while it is being read
	if m.paused {
		m.pending = append(m.pending, request)
		return
	}

	for _, target := range []*endpoint{m.endpoints[0], m.screen(request.Endpoint)} {
		target.requests = append(target.requests, request)

		// screen not being watched: count it as unread
		if target != m.current() {
			target.unread++
		}
	}

	m.cursor = len(m.visible()) - 1
	m.syncDetail()
}

// resize lays the panes out for a new terminal size.
func (m *model) resize(width, height int) {
	m.width, m.height = width, height
	m.filter.Width = width - sidebarWidth - 6

	listHeight := m.listHeight()
	detailHeight := height - footerHeight - listHeight - 2

	// terminal too short: let the viewport collapse instead of going negative
	if detailHeight < 1 {
		detailHeight = 1
	}

	m.detail.Width = width - sidebarWidth - 3
	m.detail.Height = detailHeight
	m.syncDetail()
}

// listHeight is how many request rows fit above the detail pane.
func (m *model) listHeight() int {
	half := (m.height - footerHeight - 2) / 2

	// tiny terminal: always keep room for one row
	if half < 1 {
		return 1
	}

	return half
}

// handleKey routes a key press, returning the command it produced.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// searching: the text input owns every key
	if m.searching {
		return m.handleSearchKey(msg)
	}

	key := msg.String()

	// quit keys: leave
	if key == "q" || key == "ctrl+c" {
		return tea.Quit
	}

	// known action: run it and stop
	if action, ok := keyActions[key]; ok {
		action(m)
		m.syncDetail()
		return nil
	}

	// anything else scrolls the detail pane
	updated, cmd := m.detail.Update(msg)
	m.detail = updated
	return cmd
}

// handleSearchKey feeds the filter input while the search bar is open.
func (m *model) handleSearchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.searching = false
		m.filter.SetValue("")
		m.filter.Blur()

	case "enter":
		m.searching = false
		m.filter.Blur()

	default:
		updated, cmd := m.filter.Update(msg)
		m.filter = updated
		m.clampCursor()
		m.syncDetail()
		return cmd
	}

	m.clampCursor()
	m.syncDetail()
	return nil
}

// moveUp walks up the pane holding the focus.
func (m *model) moveUp() {
	// sidebar focused: the arrows change screen
	if m.focus == paneSidebar {
		m.prevEndpoint()
		return
	}

	m.cursorUp()
}

// moveDown walks down the pane holding the focus.
func (m *model) moveDown() {
	// sidebar focused: the arrows change screen
	if m.focus == paneSidebar {
		m.nextEndpoint()
		return
	}

	m.cursorDown()
}

func (m *model) focusSidebar() {
	m.focus = paneSidebar
}

func (m *model) focusRequests() {
	m.focus = paneRequests
}

// toggleFocus swaps the pane the arrows drive.
func (m *model) toggleFocus() {
	// on the sidebar: hand the arrows to the request list
	if m.focus == paneSidebar {
		m.focus = paneRequests
		return
	}

	m.focus = paneSidebar
}

func (m *model) nextEndpoint() {
	m.selectEndpoint(m.selected + 1)
}

func (m *model) prevEndpoint() {
	m.selectEndpoint(m.selected - 1)
}

// selectEndpoint moves to another screen, wrapping around the ends.
func (m *model) selectEndpoint(index int) {
	count := len(m.endpoints)
	m.selected = ((index % count) + count) % count
	m.current().unread = 0
	m.cursor = len(m.visible()) - 1
	m.clampCursor()
}

func (m *model) cursorDown() {
	m.cursor++
	m.clampCursor()
}

func (m *model) cursorUp() {
	m.cursor--
	m.clampCursor()
}

// clampCursor keeps the cursor inside the visible rows.
func (m *model) clampCursor() {
	last := len(m.visible()) - 1

	// no rows: park the cursor at zero
	if last < 0 {
		m.cursor = 0
		return
	}

	m.cursor = min(max(m.cursor, 0), last)
}

// togglePause freezes the list, flushing what arrived while it was frozen.
func (m *model) togglePause() {
	m.paused = !m.paused

	// still paused: nothing to flush yet
	if m.paused {
		return
	}

	held := m.pending
	m.pending = nil
	for _, request := range held {
		m.add(request)
	}
}

func (m *model) startSearch() {
	m.searching = true
	m.filter.Focus()
}

func (m *model) clearFilter() {
	m.filter.SetValue("")
	m.clampCursor()
}

// clearScreen drops the captured requests of the current endpoint.
func (m *model) clearScreen() {
	m.current().requests = nil
	m.cursor = 0
}

// cycleOverride walks the current endpoint through the forced status codes.
func (m *model) cycleOverride() {
	screen := m.current()

	// the All screen is an aggregate: it has no mock to override
	if screen.name == allScreen {
		return
	}

	position := 0
	for index, status := range overrideCycle {
		// current value found: the next one wins
		if status == screen.override {
			position = index
		}
	}

	screen.override = overrideCycle[(position+1)%len(overrideCycle)]
	m.server.SetOverride(screen.name, screen.override)
}

// syncDetail refreshes the detail pane for the request under the cursor.
func (m *model) syncDetail() {
	rows := m.visible()

	// nothing to show: leave a hint instead of an empty pane
	if len(rows) == 0 || m.cursor >= len(rows) {
		m.detail.SetContent(dimStyle.Render("nenhuma request nesta tela"))
		return
	}

	m.detail.SetContent(renderDetail(rows[m.cursor], m.detail.Width))
	m.detail.GotoTop()
}

// renderDetail builds the full dump of one request.
func renderDetail(request Request, width int) string {
	lines := []string{
		titleStyle.Render(request.Method+" "+request.Path) + "  " +
			statusStyle(request.Status).Render(fmt.Sprint(request.Status)),
		dimStyle.Render(request.Time.Format("15:04:05.000")),
	}

	// query present: show it before the headers
	if request.Query != "" {
		lines = append(lines, "", titleStyle.Render("Query:"),
			"  "+clean(request.Query))
	}

	lines = append(lines, "", titleStyle.Render("Headers:"))
	lines = append(lines, renderHeaders(request.Headers)...)

	lines = append(lines, "", titleStyle.Render("Body:"))
	lines = append(lines, renderBody(request.Body)...)

	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

// renderHeaders lists the headers sorted, so the same call always looks alike.
func renderHeaders(headers map[string][]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]string, 0, len(names))
	for _, name := range names {
		rows = append(rows, "  "+dimStyle.Render(clean(name)+":")+" "+
			clean(strings.Join(headers[name], ", ")))
	}

	return rows
}

// renderBody indents and colors the body, or shows it raw when it is not JSON.
func renderBody(body string) []string {
	// empty body: say so instead of leaving a blank line
	if body == "" {
		return []string{"  " + dimStyle.Render("(vazio)")}
	}

	rows := strings.Split(colorizeJSON(clean(body)), "\n")
	for index, row := range rows {
		rows[index] = "  " + row
	}

	return rows
}

// colorizeJSON indents JSON and paints it the way jq does.
//
// ponytail: a regexp pass over already-indented JSON, no lexer. json.Indent
// has validated the text by then, so the tokens cannot be ambiguous. Swap in
// json.Decoder.Token() if colouring ever has to understand nesting.
func colorizeJSON(raw string) string {
	var indented bytes.Buffer

	// not JSON: show the text exactly as it arrived
	if err := json.Indent(&indented, []byte(raw), "", "  "); err != nil {
		return raw
	}

	return jsonToken.ReplaceAllStringFunc(indented.String(), paintToken)
}

// paintToken colors one JSON token.
func paintToken(token string) string {
	switch {
	// the key match carries its colon, which no value ever does
	case strings.HasSuffix(token, ":"):
		return keyStyle.Render(token)

	case strings.HasPrefix(token, `"`):
		return stringStyle.Render(token)

	case token == "null":
		return nullStyle.Render(token)
	}

	return token
}

// statusStyle colors a status code green below 400 and red from there up.
func statusStyle(status int) lipgloss.Style {
	// client or server error: red
	if status >= 400 {
		return failStyle
	}

	return okStyle
}

func (m *model) View() string {
	panes := lipgloss.JoinHorizontal(lipgloss.Top,
		sidebarStyle.Height(m.height-footerHeight).Render(m.renderSidebar()),
		m.renderRight())

	return panes + "\n" + m.renderFooter()
}

// paneTitle renders a pane header, dimmed when the pane has no focus.
func paneTitle(text string, focused bool) string {
	// pane not focused: fade the header so the active one stands out
	if !focused {
		return dimStyle.Render(text)
	}

	return titleStyle.Render(text)
}

// marker points at the selected row, faded when its pane has no focus.
func marker(focused bool) string {
	// pane not focused: the arrows are not driving this list
	if !focused {
		return dimStyle.Render("▸ ")
	}

	return selectStyle.Render("▸ ")
}

// renderSidebar lists every screen with its unread count.
func (m *model) renderSidebar() string {
	rows := []string{paneTitle("endpoints", m.focus == paneSidebar), ""}

	for index, screen := range m.endpoints {
		rows = append(rows, m.renderSidebarRow(index, screen))
	}

	return strings.Join(rows, "\n")
}

// renderSidebarRow draws one screen entry: marker, name, badges.
func (m *model) renderSidebarRow(index int, screen *endpoint) string {
	pointer, name := "  ", truncate(screen.name, sidebarWidth-9)

	// selected screen: mark it and highlight the name
	if index == m.selected {
		pointer = marker(m.focus == paneSidebar)
		name = selectStyle.Render(name)
	}

	badge := dimStyle.Render(fmt.Sprint(len(screen.requests)))

	// unread calls arrived: show the count instead
	if screen.unread > 0 {
		badge = unreadStyle.Render(fmt.Sprintf("%d•", screen.unread))
	}

	// status forced from here: flag it so it is never a surprise
	if screen.override != 0 {
		badge += warnStyle.Render(fmt.Sprintf(" !%d", screen.override))
	}

	return pointer + name + " " + badge
}

// renderRight stacks the request list over the detail pane.
func (m *model) renderRight() string {
	width := m.width - sidebarWidth - 3
	divider := dimStyle.Render(strings.Repeat("─", max(width, 1)))

	return lipgloss.NewStyle().PaddingLeft(1).Render(strings.Join([]string{
		m.renderList(),
		divider,
		m.detail.View(),
	}, "\n"))
}

// renderList draws the visible request rows around the cursor.
func (m *model) renderList() string {
	rows := m.visible()
	height := m.listHeight()

	header := paneTitle(truncate(m.current().name, 40),
		m.focus == paneRequests)

	// no rows: the header alone, padded so the divider stays put
	if len(rows) == 0 {
		return header + strings.Repeat("\n", height-1)
	}

	offset := max(0, min(m.cursor-height+2, len(rows)-height+1))
	lines := []string{header}

	for index := offset; index < len(rows) && len(lines) < height; index++ {
		lines = append(lines, m.renderRow(rows[index], index == m.cursor))
	}

	// short list: pad so the panes below do not jump around
	for len(lines) < height {
		lines = append(lines, "")
	}

	return strings.Join(lines, "\n")
}

// renderRow draws one request as a single line.
func (m *model) renderRow(request Request, selected bool) string {
	pointer := "  "

	// row under the cursor: point at it
	if selected {
		pointer = marker(m.focus == paneRequests)
	}

	width := m.width - sidebarWidth - 26

	return fmt.Sprintf("%s%s %s %s %s",
		pointer,
		dimStyle.Render(request.Time.Format("15:04:05")),
		request.Method,
		truncate(request.Path, max(width, 10)),
		statusStyle(request.Status).Render(fmt.Sprint(request.Status)))
}

// renderFooter shows the search bar when searching, the key hints otherwise.
func (m *model) renderFooter() string {
	// searching: the input replaces the hints
	if m.searching {
		return m.filter.View()
	}

	hints := "↑↓ navega · ←→ troca de painel · space pausa · / busca · " +
		"s status · c limpa · q sai"

	// paused: say how many calls are being held back
	if m.paused {
		return warnStyle.Render(fmt.Sprintf("PAUSADO (%d na fila) ", len(m.pending))) +
			dimStyle.Render(hints)
	}

	// filter active: keep it visible while it hides rows
	if m.filter.Value() != "" {
		return warnStyle.Render("/"+m.filter.Value()+" ") + dimStyle.Render(hints)
	}

	return dimStyle.Render(hints)
}

// truncate shortens text to width, marking the cut with an ellipsis.
func truncate(text string, width int) string {
	runes := []rune(text)

	// already fits: leave it alone
	if len(runes) <= width {
		return text
	}

	// too narrow for an ellipsis: cut hard
	if width < 2 {
		return string(runes[:max(width, 0)])
	}

	return string(runes[:width-1]) + "…"
}
