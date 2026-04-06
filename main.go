package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"
	"gopkg.in/yaml.v3"
)

type note struct {
	ID       string   `yaml:"id"`
	Title    string   `yaml:"title"`
	Type     string   `yaml:"type"`
	Tags     []string `yaml:"tags"`
	Parent   string   `yaml:"parent"`
	Children []string `yaml:"children"`
	Path     string   `yaml:"-"`
	Body     string   `yaml:"-"`
}

type edge struct {
	ID      string
	Alias   bool
	Missing bool
}

type graph struct {
	Notes          map[string]*note
	RootID         string
	PrimaryEdges   map[string][]edge
	PrimaryParent  map[string]string
	SecondaryEdges map[string][]edge
}

type line struct {
	ID          string
	Depth       int
	Alias       bool
	Missing     bool
	Expandable  bool
	Expanded    bool
	ChildCount  int
	DisplayText string
}

type model struct {
	graph         *graph
	rootID        string
	expanded      map[string]bool
	lines         []line
	selected      int
	focusRight    bool
	prefix        string
	width         int
	height        int
	ready         bool
	viewport      viewport.Model
	status        string
	mdStyle       string
	renderer      *glamour.TermRenderer
	rendererWidth int
	rendererStyle string
	noteCache     map[string]string
	refsCache     map[string][]string
	viewportKey   string
	refSourceID   string
	refIndex      int
}

func shortID(id string) string {
	if idx := strings.Index(id, "-"); idx >= 0 {
		return id[:idx]
	}
	return id
}

var (
	appStyle = lipgloss.NewStyle().Padding(0, 1)

	treePane = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240")).
			BorderRight(true).
			Padding(0, 1)
	rightPane = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240"))
	bodyPane = lipgloss.NewStyle().
			Padding(0, 1)
	refsPane = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), true, false, false, false).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	aliasStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	refStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	refActive  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	treeActive = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	treeDim    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238"))
)

var noteRefPattern = regexp.MustCompile(`\br\d{3}(?:-[a-z0-9][a-z0-9-]*)?\b`)

func main() {
	cmd := "view"
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "view", "export", "diff", "help":
			cmd = args[0]
			args = args[1:]
		}
	}

	var err error
	switch cmd {
	case "view":
		err = runView(args)
	case "export":
		err = runExport(args)
	case "diff":
		err = runDiff(args)
	case "help":
		printUsage(os.Stdout)
	default:
		err = fmt.Errorf("unknown command: %s", cmd)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "noted: %v\n", err)
		os.Exit(1)
	}
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `Usage:
  noted view   [-notes DIR] [-root ID] [-md-style STYLE]
  noted export [-notes DIR] [-o FILE] [-metadata] [--index-only] [--recurse N] NOTE_ID...
  noted diff   --base COMMIT [-notes DIR] [--index-only] [--recurse N] [NOTE_ID...]

Commands:
  view     interactive note explorer
  export   export a single combined markdown report
  diff     show note diffs against a git base commit
`)
}

func runView(args []string) error {
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	notesDir := fs.String("notes", "", "path to notes directory")
	rootID := fs.String("root", "r000-main", "root note id")
	mdStyle := fs.String("md-style", "ascii", "markdown style: ascii|notty|light|dark|auto")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  noted view [-notes DIR] [-root ID] [-md-style STYLE]

Options:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if !validMarkdownStyle(*mdStyle) {
		return fmt.Errorf("invalid -md-style %q", *mdStyle)
	}
	if *notesDir == "" {
		*notesDir = filepath.Join(".", "notes")
	}

	g, err := loadGraph(*notesDir, *rootID)
	if err != nil {
		return err
	}

	m := newModel(g, *mdStyle)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	notesDir := fs.String("notes", "", "path to notes directory")
	outPath := fs.String("o", "", "output path (default: stdout)")
	includeMetadata := fs.Bool("metadata", false, "include note metadata blocks")
	indexOnly := fs.Bool("index-only", false, "print only selected note ids in export order")
	recurseDepth := fs.Int("recurse", -1, "recursion depth: default unlimited, 0 exact-only")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  noted export [-notes DIR] [-o FILE] [-metadata] [--index-only] [--recurse N] NOTE_ID...

Behavior:
  default recurse: unlimited
  --recurse 0: export exactly NOTE_ID...
  --recurse N: recurse N levels

Options:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *recurseDepth < -1 {
		return fmt.Errorf("invalid --recurse %d", *recurseDepth)
	}
	if *notesDir == "" {
		*notesDir = filepath.Join(".", "notes")
	}
	rootIDs := fs.Args()
	if len(rootIDs) == 0 {
		return fmt.Errorf("export requires at least one NOTE_ID")
	}

	notes, err := loadNotes(*notesDir)
	if err != nil {
		return err
	}
	for _, id := range rootIDs {
		if _, ok := notes[id]; !ok {
			return fmt.Errorf("note %q not found in %s", id, *notesDir)
		}
	}
	adj := buildAdjacency(notes)
	order := resolveExportOrder(adj, rootIDs, *recurseDepth)
	var out string
	if *indexOnly {
		out = strings.Join(order, "\n")
		if out != "" {
			out += "\n"
		}
	} else {
		out = exportMarkdown(notes, adj, exportOptions{
			IncludeMetadata: *includeMetadata,
			Roots:           rootIDs,
			RecurseDepth:    *recurseDepth,
		})
	}
	if *outPath == "" {
		fmt.Print(out)
		return nil
	}
	return os.WriteFile(*outPath, []byte(out), 0o644)
}

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	notesDir := fs.String("notes", "", "path to notes directory")
	base := fs.String("base", "", "git commit/ref to diff against")
	indexOnly := fs.Bool("index-only", false, "print only changed note ids in diff order")
	recurseDepth := fs.Int("recurse", -1, "recursion depth for NOTE_ID filters: default unlimited, 0 exact-only")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  noted diff --base COMMIT [-notes DIR] [--index-only] [--recurse N] [NOTE_ID...]

Behavior:
  NOTE_ID omitted: include all changed notes
  NOTE_ID provided: default recurse is unlimited
  --recurse 0: diff exactly NOTE_ID...
  --recurse N: recurse N levels from NOTE_ID...

Options:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *base == "" {
		return fmt.Errorf("diff requires --base COMMIT")
	}
	if *recurseDepth < -1 {
		return fmt.Errorf("invalid --recurse %d", *recurseDepth)
	}
	if *notesDir == "" {
		*notesDir = filepath.Join(".", "notes")
	}

	notes, err := loadNotes(*notesDir)
	if err != nil {
		return err
	}
	repoRoot, err := gitRepoRoot(*notesDir)
	if err != nil {
		return err
	}
	if err := validateGitBase(repoRoot, *base); err != nil {
		return err
	}

	order, err := resolveDiffOrder(notes, repoRoot, *base, fs.Args(), *recurseDepth)
	if err != nil {
		return err
	}
	if *indexOnly {
		out := strings.Join(order, "\n")
		if out != "" {
			out += "\n"
		}
		fmt.Print(out)
		return nil
	}

	parts := make([]string, 0, len(order))
	for _, id := range order {
		n := notes[id]
		relPath, err := filepath.Rel(repoRoot, n.Path)
		if err != nil {
			return err
		}
		diff, err := gitDiffPath(repoRoot, *base, relPath)
		if err != nil {
			return err
		}
		if strings.TrimSpace(diff) == "" {
			continue
		}
		parts = append(parts, strings.TrimRight(diff, "\n"))
	}
	if len(parts) == 0 {
		return nil
	}
	fmt.Println(strings.Join(parts, "\n\n"))
	return nil
}

func loadGraph(notesDir, rootID string) (*graph, error) {
	notes, err := loadNotes(notesDir)
	if err != nil {
		return nil, err
	}
	if _, ok := notes[rootID]; !ok {
		return nil, fmt.Errorf("root note %q not found in %s", rootID, notesDir)
	}

	adj := buildAdjacency(notes)
	primary, parent, secondary := buildTraversal(adj, rootID)

	return &graph{
		Notes:          notes,
		RootID:         rootID,
		PrimaryEdges:   primary,
		PrimaryParent:  parent,
		SecondaryEdges: secondary,
	}, nil
}

type exportOptions struct {
	IncludeMetadata bool
	Roots           []string
	RecurseDepth    int
}

func resolveExportOrder(adj map[string][]string, roots []string, recurseDepth int) []string {
	order := []string{}
	seen := map[string]bool{}

	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if seen[id] {
			return
		}
		seen[id] = true
		order = append(order, id)
		if recurseDepth == 0 || (recurseDepth > 0 && depth >= recurseDepth) {
			return
		}
		for _, child := range exportChildren(adj, id) {
			walk(child, depth+1)
		}
	}

	for _, id := range roots {
		walk(id, 0)
	}
	return order
}

func resolveDiffOrder(notes map[string]*note, repoRoot, base string, roots []string, recurseDepth int) ([]string, error) {
	if len(roots) > 0 {
		for _, id := range roots {
			if _, ok := notes[id]; !ok {
				return nil, fmt.Errorf("note %q not found", id)
			}
		}
		adj := buildAdjacency(notes)
		order := resolveExportOrder(adj, roots, recurseDepth)
		return filterChangedNotes(notes, repoRoot, base, order)
	}

	ids := make([]string, 0, len(notes))
	for id := range notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return filterChangedNotes(notes, repoRoot, base, ids)
}

func filterChangedNotes(notes map[string]*note, repoRoot, base string, ids []string) ([]string, error) {
	changed := make([]string, 0, len(ids))
	for _, id := range ids {
		n, ok := notes[id]
		if !ok {
			continue
		}
		relPath, err := filepath.Rel(repoRoot, n.Path)
		if err != nil {
			return nil, err
		}
		differs, err := noteChanged(repoRoot, base, relPath)
		if err != nil {
			return nil, err
		}
		if differs {
			changed = append(changed, id)
		}
	}
	return changed, nil
}

func gitRepoRoot(path string) (string, error) {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve git repo root: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func validateGitBase(repoRoot, base string) error {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", base+"^{commit}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid base %q: %s", base, strings.TrimSpace(string(out)))
	}
	return nil
}

func noteChanged(repoRoot, base, relPath string) (bool, error) {
	cmd := exec.Command("git", "-C", repoRoot, "diff", "--quiet", base, "--", relPath)
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

func gitDiffPath(repoRoot, base, relPath string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "diff", base, "--", relPath).CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func exportMarkdown(notes map[string]*note, adj map[string][]string, opts exportOptions) string {
	var b strings.Builder
	seen := map[string]bool{}

	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		n, ok := notes[id]
		if !ok || seen[id] {
			return
		}
		seen[id] = true

		level := min(depth+1, 6)
		fmt.Fprintf(&b, "<a id=\"%s\"></a>\n\n", n.ID)
		fmt.Fprintf(&b, "%s %s | %s\n\n", strings.Repeat("#", level), shortID(n.ID), n.Title)
		if opts.IncludeMetadata {
			meta := []string{
				fmt.Sprintf("- id: `%s`", n.ID),
				fmt.Sprintf("- path: `%s`", n.Path),
			}
			if n.Type != "" {
				meta = append(meta, fmt.Sprintf("- type: `%s`", n.Type))
			}
			if n.Parent != "" && n.Parent != "null" {
				meta = append(meta, fmt.Sprintf("- parent: `%s`", n.Parent))
			}
			if len(n.Tags) > 0 {
				meta = append(meta, fmt.Sprintf("- tags: `%s`", strings.Join(n.Tags, "`, `")))
			}
			if len(n.Children) > 0 {
				meta = append(meta, fmt.Sprintf("- children: `%s`", strings.Join(n.Children, "`, `")))
			}
			b.WriteString("**Metadata**\n\n")
			b.WriteString(strings.Join(meta, "\n"))
			b.WriteString("\n\n")
		}

		body := strings.TrimSpace(rewriteNoteLinks(n.Body, notes))
		if body != "" {
			b.WriteString(body)
			b.WriteString("\n\n")
		}

		if opts.RecurseDepth != 0 && (opts.RecurseDepth < 0 || depth < opts.RecurseDepth) {
			repeats := []string{}
			for _, child := range exportChildren(adj, id) {
				if seen[child] {
					repeats = append(repeats, child)
					continue
				}
				walk(child, depth+1)
			}
			if len(repeats) > 0 {
				labels := make([]string, 0, len(repeats))
				for _, child := range repeats {
					if n, ok := notes[child]; ok {
						labels = append(labels, fmt.Sprintf("[%s](#%s)", shortID(n.ID), n.ID))
					}
				}
				if len(labels) > 0 {
					b.WriteString("Already covered: ")
					b.WriteString(strings.Join(labels, ", "))
					b.WriteString("\n\n")
				}
			}
		}
	}

	for _, id := range opts.Roots {
		walk(id, 0)
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func exportChildren(adj map[string][]string, id string) []string {
	children := []string{}
	seen := map[string]bool{}
	for _, child := range adj[id] {
		if seen[child] {
			continue
		}
		seen[child] = true
		children = append(children, child)
	}
	return children
}

func rewriteNoteLinks(text string, notes map[string]*note) string {
	return regexp.MustCompile(`\]\(([^)]+)\)`).ReplaceAllStringFunc(text, func(match string) string {
		parts := regexp.MustCompile(`\]\(([^)]+)\)`).FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		target := parts[1]
		if !strings.HasSuffix(target, ".md") {
			return match
		}
		base := strings.TrimSuffix(filepath.Base(target), ".md")
		if _, ok := notes[base]; !ok {
			return match
		}
		return strings.Replace(match, "("+target+")", "(#"+base+")", 1)
	})
}

func loadNotes(notesDir string) (map[string]*note, error) {
	notesDir, err := filepath.Abs(notesDir)
	if err != nil {
		return nil, err
	}
	notes := map[string]*note{}
	err = filepath.WalkDir(notesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "_archive" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}

		n, err := parseNote(path)
		if err != nil {
			if strings.Contains(err.Error(), "expected YAML frontmatter") {
				return nil
			}
			return err
		}
		if n.ID == "" {
			return fmt.Errorf("missing note id in %s", path)
		}
		notes[n.ID] = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return notes, nil
}

func parseNote(path string) (*note, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	text := string(raw)
	parts := strings.SplitN(text, "---\n", 3)
	if len(parts) < 3 || parts[0] != "" {
		return nil, fmt.Errorf("expected YAML frontmatter in %s", path)
	}

	var n note
	if err := yaml.Unmarshal([]byte(parts[1]), &n); err != nil {
		return nil, fmt.Errorf("parse frontmatter %s: %w", path, err)
	}
	n.Path = path
	n.Body = strings.TrimSpace(parts[2])
	return &n, nil
}

func buildAdjacency(notes map[string]*note) map[string][]string {
	adj := map[string][]string{}
	parentExtras := map[string][]string{}
	for id := range notes {
		adj[id] = nil
	}

	for id, n := range notes {
		seen := map[string]bool{}
		for _, child := range n.Children {
			child = strings.TrimSpace(child)
			if child == "" || seen[child] {
				continue
			}
			adj[id] = append(adj[id], child)
			seen[child] = true
		}
		if n.Parent != "" && n.Parent != "null" {
			parentExtras[n.Parent] = append(parentExtras[n.Parent], id)
		}
	}

	for parent, extras := range parentExtras {
		seen := map[string]bool{}
		for _, child := range adj[parent] {
			seen[child] = true
		}
		sort.Strings(extras)
		for _, child := range extras {
			if seen[child] {
				continue
			}
			adj[parent] = append(adj[parent], child)
		}
	}

	return adj
}

func buildTraversal(adj map[string][]string, rootID string) (map[string][]edge, map[string]string, map[string][]edge) {
	primary := map[string][]edge{}
	parent := map[string]string{}
	secondary := map[string][]edge{}
	queue := []string{rootID}
	seen := map[string]bool{rootID: true}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, child := range adj[current] {
			if !seen[child] {
				primary[current] = append(primary[current], edge{ID: child})
				parent[child] = current
				seen[child] = true
				queue = append(queue, child)
				continue
			}
			secondary[current] = append(secondary[current], edge{ID: child, Alias: true})
		}
	}

	for p, children := range adj {
		for _, child := range children {
			if seen[child] {
				continue
			}
			primary[p] = append(primary[p], edge{ID: child})
			if _, ok := parent[child]; !ok {
				parent[child] = p
			}
			seen[child] = true
		}
	}

	return primary, parent, secondary
}

func newModel(g *graph, mdStyle string) model {
	m := model{
		graph:     g,
		rootID:    g.RootID,
		expanded:  map[string]bool{g.RootID: true},
		mdStyle:   mdStyle,
		noteCache: map[string]string{},
		refsCache: map[string][]string{},
		status:    "arrows/jk move | [count]+right expand N levels | left collapse | enter focus viewer | tab cycle refs | enter open ref | esc back | e edit note | q quit",
	}
	m.rebuildLines()
	return m
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.resize()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "e":
			m.prefix = ""
			return m, m.openEditor()
		}

		if m.focusRight {
			switch msg.String() {
			case "esc":
				m.focusRight = false
			case "tab":
				m.cycleReference(1)
			case "shift+tab":
				m.cycleReference(-1)
			case "enter":
				m.followReference()
			case "up", "k":
				m.viewport.LineUp(1)
			case "down", "j":
				m.viewport.LineDown(1)
			case "pgup", "b":
				m.viewport.HalfViewUp()
			case "pgdown", "f":
				m.viewport.HalfViewDown()
			case "g":
				m.viewport.GotoTop()
			case "G":
				m.viewport.GotoBottom()
			}
			return m, nil
		}

		if len(msg.String()) == 1 && msg.String()[0] >= '0' && msg.String()[0] <= '9' {
			if m.prefix != "" || msg.String() != "0" {
				m.prefix += msg.String()
				return m, nil
			}
		}

		switch msg.String() {
		case "up", "k":
			m.prefix = ""
			if m.selected > 0 {
				m.selected--
				m.syncViewport()
			}
		case "down", "j":
			m.prefix = ""
			if m.selected < len(m.lines)-1 {
				m.selected++
				m.syncViewport()
			}
		case "left", "h":
			m.prefix = ""
			m.collapseCurrent()
		case "right", "l", " ":
			levels := 1
			if m.prefix != "" {
				fmt.Sscanf(m.prefix, "%d", &levels)
			}
			m.prefix = ""
			m.expandCurrent(levels)
		case "enter":
			m.prefix = ""
			m.focusRight = true
			m.syncViewport()
		case "g":
			m.prefix = ""
			m.selected = 0
			m.syncViewport()
		case "G":
			m.prefix = ""
			if len(m.lines) > 0 {
				m.selected = len(m.lines) - 1
				m.syncViewport()
			}
		case "esc":
			m.prefix = ""
		}
		return m, nil
	}

	return m, nil
}

func (m model) openEditor() tea.Cmd {
	if len(m.lines) == 0 {
		return nil
	}

	note, ok := m.graph.Notes[m.lines[m.selected].ID]
	if !ok {
		return nil
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		return nil
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell, "-lc", editor+" "+shellQuote(note.Path))
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return nil })
}

func (m *model) collapseCurrent() {
	if len(m.lines) == 0 {
		return
	}

	cur := m.lines[m.selected]
	if cur.Expanded {
		delete(m.expanded, cur.ID)
		m.rebuildLines()
		m.syncViewport()
		return
	}

	for i := m.selected - 1; i >= 0; i-- {
		if m.lines[i].Depth < cur.Depth {
			m.selected = i
			m.syncViewport()
			return
		}
	}
}

func (m *model) expandCurrent(levels int) {
	if len(m.lines) == 0 {
		return
	}
	if levels < 1 {
		levels = 1
	}

	cur := m.lines[m.selected]
	if cur.Missing {
		return
	}
	if cur.Alias {
		for i, line := range m.lines {
			if line.ID == cur.ID && !line.Alias {
				m.selected = i
				m.syncViewport()
				return
			}
		}
	}
	if !cur.Expandable {
		return
	}
	m.expandLevels(cur.ID, levels)
	m.rebuildLines()
	m.syncViewport()
}

func (m *model) expandLevels(id string, levels int) {
	if levels <= 0 {
		return
	}
	m.expanded[id] = true
	if levels == 1 {
		return
	}
	for _, child := range m.graph.PrimaryEdges[id] {
		if _, ok := m.graph.Notes[child.ID]; !ok {
			continue
		}
		m.expandLevels(child.ID, levels-1)
	}
	for _, child := range m.graph.SecondaryEdges[id] {
		if _, ok := m.graph.Notes[child.ID]; !ok {
			continue
		}
		m.expandLevels(child.ID, levels-1)
	}
}

func (m *model) resize() {
	if !m.ready {
		return
	}
	leftWidth := max(28, m.width/3)
	rightWidth := max(40, m.width-leftWidth-4)
	contentHeight := max(8, m.height-4)
	rightInnerHeight := max(6, contentHeight-2)
	refsTotalHeight := 5
	if rightInnerHeight < 10 {
		refsTotalHeight = 4
	}
	bodyHeight := max(4, rightInnerHeight-refsTotalHeight)
	refsTotalHeight = rightInnerHeight - bodyHeight
	refsContentHeight := max(1, refsTotalHeight-1)
	m.viewport.Width = rightWidth - 2
	m.viewport.Height = bodyHeight
	m.viewportKey = ""

	treePane = treePane.Width(leftWidth).Height(contentHeight)
	rightPane = rightPane.Width(rightWidth).Height(contentHeight)
	bodyPane = bodyPane.Width(rightWidth - 2).Height(bodyHeight)
	refsPane = refsPane.Width(rightWidth - 2).Height(refsContentHeight)
	m.syncViewport()
}

func (m *model) syncViewport() {
	if !m.ready || len(m.lines) == 0 {
		return
	}
	id := m.lines[m.selected].ID
	if id != m.refSourceID {
		m.refSourceID = id
		m.refIndex = 0
	}
	key := m.noteCacheKey(id)
	if key == m.viewportKey {
		return
	}
	content, ok := m.noteCache[key]
	if !ok {
		content = m.renderNote(id)
		m.noteCache[key] = content
	}
	m.viewport.SetContent(content)
	m.viewport.GotoTop()
	m.viewportKey = key
}

func (m *model) rebuildLines() {
	lines := []line{}
	seenPrimary := map[string]bool{}

	var walk func(id string, depth int, alias bool)
	walk = func(id string, depth int, alias bool) {
		note, ok := m.graph.Notes[id]
		display := shortID(id)
		if ok && note.Title != "" {
			display = fmt.Sprintf("%s | %s", shortID(note.ID), note.Title)
		}
		line := line{
			ID:          id,
			Depth:       depth,
			Alias:       alias,
			Missing:     !ok,
			Expandable:  len(m.graph.PrimaryEdges[id]) > 0 || len(m.graph.SecondaryEdges[id]) > 0,
			Expanded:    m.expanded[id],
			ChildCount:  len(m.graph.PrimaryEdges[id]) + len(m.graph.SecondaryEdges[id]),
			DisplayText: display,
		}
		lines = append(lines, line)

		if alias || !line.Expanded || !ok {
			return
		}
		if seenPrimary[id] {
			return
		}
		seenPrimary[id] = true

		for _, child := range m.graph.PrimaryEdges[id] {
			walk(child.ID, depth+1, child.Alias)
		}
		for _, child := range m.graph.SecondaryEdges[id] {
			walk(child.ID, depth+1, true)
		}
	}

	walk(m.rootID, 0, false)
	if m.selected >= len(lines) {
		m.selected = max(0, len(lines)-1)
	}
	m.lines = lines
}

func (m model) View() string {
	if !m.ready {
		return "loading..."
	}

	left := m.renderTree()
	right := m.viewport.View()
	refs := m.renderReferences()

	leftStyle := treePane.Copy()
	rightStyle := rightPane.Copy()
	bodyStyle := bodyPane.Copy()
	refsStyle := refsPane.Copy()
	if m.focusRight {
		rightStyle = rightStyle.BorderForeground(lipgloss.Color("42"))
		refsStyle = refsStyle.BorderForeground(lipgloss.Color("42"))
	} else {
		leftStyle = leftStyle.BorderForeground(lipgloss.Color("42"))
	}
	leftBox := leftStyle.Render(left)
	rightInner := lipgloss.JoinVertical(
		lipgloss.Left,
		bodyStyle.Render(right),
		refsStyle.Render(refs),
	)
	rightBox := rightStyle.Render(rightInner)

	footer := helpStyle.Render(m.status)
	if m.prefix != "" && !m.focusRight {
		footer = helpStyle.Render(fmt.Sprintf("pending count: %s | %s", m.prefix, m.status))
	}
	return appStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Left,
			lipgloss.JoinHorizontal(
				lipgloss.Top,
				leftBox,
				rightBox,
			),
			footer,
		),
	)
}

func (m model) renderTree() string {
	var b strings.Builder
	for i, line := range m.lines {
		prefix := strings.Repeat("  ", line.Depth)
		marker := "•"
		switch {
		case line.Missing:
			marker = "!"
		case line.Expandable && line.Expanded:
			marker = "▾"
		case line.Expandable:
			marker = "▸"
		}

		label := line.DisplayText
		if line.Alias {
			label = aliasStyle.Render(label + " [alias]")
		} else if line.Missing {
			label = aliasStyle.Render(label + " [missing]")
		}

		row := fmt.Sprintf("%s%s %s", prefix, marker, label)
		if i == m.selected {
			if m.focusRight {
				row = treeDim.Render(row)
			} else {
				row = treeActive.Render(row)
			}
		}
		b.WriteString(row)
		if i < len(m.lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (m model) renderNote(id string) string {
	n, ok := m.graph.Notes[id]
	if !ok {
		return fmt.Sprintf("Missing note: %s", id)
	}

	width := max(30, m.viewport.Width-2)
	var parts []string
	parts = append(parts, titleStyle.Render(n.Title))

	body := n.Body
	if body == "" {
		body = "(empty note body)"
	}
	parts = append(parts, m.renderMarkdown(body, width))

	if aliases := m.aliasParents(id); len(aliases) > 0 {
		parts = append(parts, aliasStyle.Render("Also referenced from: "+strings.Join(aliases, ", ")))
	}

	return strings.Join(parts, "\n\n")
}

func (m *model) renderMarkdown(text string, width int) string {
	renderer, err := m.markdownRenderer(width)
	if err != nil {
		return wrap.String(text, width)
	}

	rendered, err := renderer.Render(text)
	if err != nil {
		return wrap.String(text, width)
	}
	return strings.TrimSpace(rendered)
}

func (m model) renderReferences() string {
	if len(m.lines) == 0 {
		return "References\n\nNo note references"
	}

	id := m.lines[m.selected].ID
	refs := m.referencesFor(id)
	if len(refs) == 0 {
		return "References\n\nNo note references"
	}

	idx := m.refIndex
	if idx >= len(refs) {
		idx = len(refs) - 1
	}
	if idx < 0 {
		idx = 0
	}

	start := max(0, idx-1)
	end := min(len(refs), start+3)
	if end-start < 3 {
		start = max(0, end-3)
	}

	lines := []string{"References"}
	for i := start; i < end; i++ {
		label := m.referenceLabel(refs[i])
		if i == idx {
			label = refActive.Render(label)
		} else {
			label = refStyle.Render(label)
		}
		lines = append(lines, label)
	}

	lines = append(lines, dimStyle.Render(fmt.Sprintf("%d/%d", idx+1, len(refs))))
	return strings.Join(lines, "\n")
}

func (m *model) markdownRenderer(width int) (*glamour.TermRenderer, error) {
	style := m.mdStyle
	if m.renderer != nil && m.rendererWidth == width && m.rendererStyle == style {
		return m.renderer, nil
	}
	if m.renderer != nil {
		_ = m.renderer.Close()
	}
	options := []glamour.TermRendererOption{glamour.WithWordWrap(width)}
	if style == "auto" {
		options = append(options, glamour.WithEnvironmentConfig())
	} else {
		options = append(options, glamour.WithStandardStyle(style))
	}
	renderer, err := glamour.NewTermRenderer(options...)
	if err != nil {
		return nil, err
	}
	m.renderer = renderer
	m.rendererWidth = width
	m.rendererStyle = style
	return renderer, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func validMarkdownStyle(style string) bool {
	switch style {
	case "auto", "ascii", "notty", "light", "dark":
		return true
	default:
		return false
	}
}

func (m model) noteCacheKey(id string) string {
	return fmt.Sprintf("%s|%d|%s", id, max(30, m.viewport.Width-2), m.mdStyle)
}

func (m *model) cycleReference(delta int) {
	if len(m.lines) == 0 {
		return
	}
	refs := m.referencesFor(m.lines[m.selected].ID)
	if len(refs) == 0 {
		return
	}
	m.refIndex = (m.refIndex + delta + len(refs)) % len(refs)
}

func (m *model) followReference() {
	if len(m.lines) == 0 {
		return
	}
	refs := m.referencesFor(m.lines[m.selected].ID)
	if len(refs) == 0 {
		return
	}
	idx := m.refIndex
	if idx < 0 || idx >= len(refs) {
		idx = 0
	}
	m.selectNote(refs[idx])
	m.focusRight = true
	m.syncViewport()
}

func (m *model) selectNote(id string) {
	if _, ok := m.graph.Notes[id]; !ok {
		return
	}
	path := []string{id}
	for cur := id; cur != m.rootID; {
		parent, ok := m.graph.PrimaryParent[cur]
		if !ok || parent == "" {
			break
		}
		path = append(path, parent)
		cur = parent
	}
	for i := len(path) - 1; i >= 0; i-- {
		m.expanded[path[i]] = true
	}
	m.rebuildLines()
	for i, line := range m.lines {
		if line.ID == id && !line.Alias {
			m.selected = i
			return
		}
	}
	for i, line := range m.lines {
		if line.ID == id {
			m.selected = i
			return
		}
	}
}

func (m *model) referencesFor(id string) []string {
	if refs, ok := m.refsCache[id]; ok {
		return refs
	}

	n, ok := m.graph.Notes[id]
	if !ok {
		return nil
	}

	seen := map[string]bool{}
	refs := []string{}
	for _, token := range noteRefPattern.FindAllString(n.Body, -1) {
		resolved, ok := m.resolveNoteRef(token)
		if !ok || resolved == id || seen[resolved] {
			continue
		}
		seen[resolved] = true
		refs = append(refs, resolved)
	}
	m.refsCache[id] = refs
	return refs
}

func (m model) resolveNoteRef(token string) (string, bool) {
	if _, ok := m.graph.Notes[token]; ok {
		return token, true
	}
	var match string
	for id := range m.graph.Notes {
		if shortID(id) == token || strings.HasPrefix(id, token+"-") {
			if match != "" && match != id {
				return "", false
			}
			match = id
		}
	}
	return match, match != ""
}

func (m model) referenceLabel(id string) string {
	n, ok := m.graph.Notes[id]
	if !ok {
		return shortID(id)
	}
	return fmt.Sprintf("%s | %s", shortID(n.ID), n.Title)
}

func (m model) aliasParents(target string) []string {
	parents := []string{}
	for parent, edges := range m.graph.SecondaryEdges {
		for _, e := range edges {
			if e.ID == target {
				parents = append(parents, parent)
			}
		}
	}
	sort.Strings(parents)
	return parents
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func init() {
	if _, err := os.Stat("/dev/tty"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
}
