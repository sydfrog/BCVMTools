//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// sheetModel is a virtual, sortable, filterable table model over a Sheet.
// Sorting and row removal modify sheet.Rows, so exports reflect what is on screen
// (minus any active filter, which only hides rows until "Keep only filtered rows").
type sheetModel struct {
	walk.TableModelBase
	walk.SorterBase
	sheet   *Sheet
	orig    [][]any
	view    []int // indexes into sheet.Rows that are visible
	query   string
	filters map[int]map[string]bool // per-column allowed values (Excel-style); nil = no filter
	sorted  bool
}

func newSheetModel(s *Sheet) *sheetModel {
	m := &sheetModel{sheet: s, filters: map[int]map[string]bool{}}
	m.orig = append([][]any(nil), s.Rows...)
	m.applyFilter()
	return m
}

func (m *sheetModel) RowCount() int { return len(m.view) }

func cellStr(r []any, c int) string {
	if c < len(r) && r[c] != nil {
		return fmt.Sprint(r[c])
	}
	return ""
}

func (m *sheetModel) Value(row, col int) interface{} {
	return cellStr(m.sheet.Rows[m.view[row]], col)
}

func (m *sheetModel) matches(r []any, q string) bool {
	for c, allowed := range m.filters {
		if !allowed[cellStr(r, c)] {
			return false
		}
	}
	if q == "" {
		return true
	}
	for c := range r {
		if strings.Contains(strings.ToLower(cellStr(r, c)), q) {
			return true
		}
	}
	return false
}

func (m *sheetModel) applyFilter() {
	q := strings.ToLower(m.query)
	m.view = m.view[:0]
	for i, r := range m.sheet.Rows {
		if m.matches(r, q) {
			m.view = append(m.view, i)
		}
	}
	m.PublishRowsReset()
}

func (m *sheetModel) Filtered() bool { return m.query != "" || len(m.filters) > 0 }

func (m *sheetModel) SetSearch(q string) {
	m.query = q
	m.applyFilter()
}

// SetColumnFilter sets the allowed values for a column (nil clears it).
func (m *sheetModel) SetColumnFilter(col int, allowed map[string]bool) {
	if allowed == nil {
		delete(m.filters, col)
	} else {
		m.filters[col] = allowed
	}
	m.applyFilter()
}

func (m *sheetModel) ClearFilters() {
	m.query = ""
	m.filters = map[int]map[string]bool{}
	m.applyFilter()
}

// StyleCell shades every second row (zebra striping) to make wide tables easier to read.
func (m *sheetModel) StyleCell(st *walk.CellStyle) {
	if st.Row()%2 == 1 {
		st.BackgroundColor = walk.RGB(236, 242, 249)
	}
}

// autoWidth picks a column width (px) from the content, capped, with a small
// contribution from the heading so long headings do not force wide columns.
func autoWidth(s *Sheet, c int) int {
	n := min(len(s.Header[c]), 12)
	for _, r := range s.Rows {
		if l := len(cellStr(r, c)); l > n {
			n = l
		}
	}
	return max(70, min(n, 55)*7+46)
}

// Sort implements walk.Sorter: numeric columns sort numerically, others case-insensitively.
func (m *sheetModel) Sort(col int, order walk.SortOrder) error {
	m.SorterBase.Sort(col, order)
	m.sorted = true
	type key struct {
		row []any
		f   float64
		num bool
		s   string
	}
	keys := make([]key, len(m.sheet.Rows))
	for i, r := range m.sheet.Rows {
		str := cellStr(r, col)
		f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
		keys[i] = key{r, f, err == nil && str != "", strings.ToLower(str)}
	}
	cmp := func(a, b key) int {
		switch {
		case a.num && b.num:
			if a.f < b.f {
				return -1
			} else if a.f > b.f {
				return 1
			}
			return 0
		case a.num != b.num: // numbers before text
			if a.num {
				return -1
			}
			return 1
		}
		return strings.Compare(a.s, b.s)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		c := cmp(keys[i], keys[j])
		if order == walk.SortDescending {
			return c > 0
		}
		return c < 0
	})
	for i := range keys {
		m.sheet.Rows[i] = keys[i].row
	}
	m.applyFilter()
	return nil
}

func (m *sheetModel) removeWhere(drop func(sheetIdx int) bool) {
	kept := m.sheet.Rows[:0:0]
	for i, r := range m.sheet.Rows {
		if !drop(i) {
			kept = append(kept, r)
		}
	}
	m.sheet.Rows = kept
	m.applyFilter()
}

func (m *sheetModel) RemoveRows(viewRows []int) {
	drop := map[int]bool{}
	for _, v := range viewRows {
		if v >= 0 && v < len(m.view) {
			drop[m.view[v]] = true
		}
	}
	m.removeWhere(func(i int) bool { return drop[i] })
}

func (m *sheetModel) KeepFiltered() {
	keep := map[int]bool{}
	for _, i := range m.view {
		keep[i] = true
	}
	m.removeWhere(func(i int) bool { return !keep[i] })
	m.ClearFilters()
}

func (m *sheetModel) Restore() {
	m.sheet.Rows = append([][]any(nil), m.orig...)
	if m.sorted {
		m.Sort(m.SortedColumn(), m.SortOrder())
		return
	}
	m.applyFilter()
}

// ---- look & feel -----------------------------------------------------------

var (
	uiFont, uiBold *walk.Font
	muted          = walk.RGB(96, 106, 120)
)

type settings struct {
	Server string      `json:"server"`
	User   string      `json:"user"`
	Verify bool        `json:"verifyTLS"`
	Width  int         `json:"width"`
	Height int         `json:"height"`
	Extra  []savedConn `json:"extra,omitempty"` // additional vCenters (never passwords)
}

type savedConn struct {
	Server string `json:"server"`
	User   string `json:"user"`
	Source string `json:"source"`
}

func settingsPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "BCVMTools", "settings.json")
}

func loadSettings() (s settings) {
	if p := settingsPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &s)
		}
	}
	return
}

func saveSettings(s settings) {
	if p := settingsPath(); p != "" {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if b, err := json.Marshal(s); err == nil {
			os.WriteFile(p, b, 0o644)
		}
	}
}

// appIcon draws a small "server stack" icon so the window has a recognisable identity.
func appIcon() *walk.Icon {
	const n = 64
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	blue, white, green := color.RGBA{47, 109, 181, 255}, color.RGBA{255, 255, 255, 255}, color.RGBA{102, 205, 120, 255}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			// rounded square
			dx, dy := max(0, max(8-x, x-(n-9))), max(0, max(8-y, y-(n-9)))
			if dx*dx+dy*dy <= 64 {
				img.Set(x, y, blue)
			}
		}
	}
	for i := 0; i < 3; i++ {
		y0 := 12 + i*16
		for y := y0; y < y0+11; y++ {
			for x := 12; x < 52; x++ {
				img.Set(x, y, white)
			}
		}
		for y := y0 + 3; y < y0+8; y++ {
			for x := 43; x < 48; x++ {
				img.Set(x, y, green)
			}
		}
	}
	ic, err := walk.NewIconFromImageForDPI(img, 96)
	if err != nil {
		return nil
	}
	return ic
}

func box(parent walk.Container, l walk.Layout) *walk.Composite {
	c, _ := walk.NewComposite(parent)
	c.SetLayout(l)
	return c
}

func hbox(margins walk.Margins, spacing int) *walk.BoxLayout {
	l := walk.NewHBoxLayout()
	l.SetMargins(margins)
	l.SetSpacing(spacing)
	return l
}

func vbox(margins walk.Margins, spacing int) *walk.BoxLayout {
	l := walk.NewVBoxLayout()
	l.SetMargins(margins)
	l.SetSpacing(spacing)
	return l
}

func button(parent walk.Container, text, tip string, minW int) *walk.PushButton {
	b, _ := walk.NewPushButton(parent)
	b.SetText(text)
	b.SetToolTipText(tip)
	b.SetMinMaxSize(walk.Size{Width: minW, Height: 28}, walk.Size{})
	return b
}

// field builds "caption above input", the layout recommended for compact forms.
func field(parent walk.Container, caption string, width int, password bool, value string) *walk.LineEdit {
	c := box(parent, vbox(walk.Margins{}, 3))
	l, _ := walk.NewLabel(c)
	l.SetText(caption)
	l.SetTextColor(muted)
	e, _ := walk.NewLineEdit(c)
	e.SetPasswordMode(password)
	e.SetText(value)
	e.SetMinMaxSize(walk.Size{Width: width, Height: 24}, walk.Size{Width: width})
	return e
}

func runGUI() error {
	var (
		sheets []*Sheet
		models []*sheetModel
	)
	// Make sure the per-user data folder exists and is writable before doing anything else.
	dir, err := appDataDir()
	if err != nil {
		walk.MsgBox(nil, "BCVMTools cannot start",
			err.Error()+"\n\nBCVMTools stores its settings and crash log in this folder. "+
				"Fix the folder permissions (or ask your administrator) and start the application again.",
			walk.MsgBoxIconError)
		return err
	}
	captureFatalCrashes(dir)
	set := loadSettings()
	var extra []Conn
	for _, s := range set.Extra {
		extra = append(extra, Conn{Server: s.Server, User: s.User, Source: s.Source})
	}
	if env := os.Getenv("BCVMTOOLS_EXTRA"); env != "" { // test hook: comma separated hosts
		for _, h := range strings.Split(env, ",") {
			extra = append(extra, Conn{Server: strings.TrimSpace(h), Source: "Manual"})
		}
	}
	if set.Width < 1000 || set.Height < 600 {
		set.Width, set.Height = 1360, 820
	}
	uiFont, _ = walk.NewFont("Segoe UI", 9, 0)
	uiBold, _ = walk.NewFont("Segoe UI", 9, walk.FontBold)

	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	mw.SetTitle("BCVMTools - VMware Inventory")
	if ic := appIcon(); ic != nil {
		mw.SetIcon(ic)
	}
	if uiFont != nil {
		mw.SetFont(uiFont)
	}
	mw.SetMinMaxSize(walk.Size{Width: 1000, Height: 600}, walk.Size{})
	mw.SetLayout(vbox(walk.Margins{HNear: 12, VNear: 10, HFar: 12, VFar: 6}, 10))

	// ---- connection
	gb, _ := walk.NewGroupBox(mw)
	gb.SetTitle("Connection")
	gb.SetLayout(hbox(walk.Margins{HNear: 10, VNear: 8, HFar: 10, VFar: 10}, 14))
	server := field(gb, "vCenter or ESXi host", 280, false, firstNonEmpty(set.Server, os.Getenv("VI_SERVER")))
	user := field(gb, "User name", 220, false, firstNonEmpty(set.User, os.Getenv("VI_USER")))
	pass := field(gb, "Password", 190, true, os.Getenv("VI_PASSWORD"))
	opts := box(gb, vbox(walk.Margins{}, 3))
	sp, _ := walk.NewLabel(opts)
	sp.SetText(" ")
	verify, _ := walk.NewCheckBox(opts)
	verify.SetText("Verify TLS certificate")
	verify.SetChecked(set.Verify)
	verify.SetToolTipText("Leave unticked for self-signed certificates (the usual vCenter default)")
	btnBox := box(gb, vbox(walk.Margins{}, 3))
	sp2, _ := walk.NewLabel(btnBox)
	sp2.SetText(" ")
	collectBtn := button(btnBox, "Collect", "Connect and gather the inventory (Enter / F5)", 120)
	if uiBold != nil {
		collectBtn.SetFont(uiBold)
	}
	moreBox := box(gb, vbox(walk.Margins{}, 3))
	sp3, _ := walk.NewLabel(moreBox)
	sp3.SetText(" ")
	moreBtn := button(moreBox, "More vCenters / VCF...", "Collect from several vCenters at once, or discover them from a VCF SDDC Manager", 170)
	walk.NewHSpacer(gb)

	progress, _ := walk.NewProgressBar(mw)
	progress.SetMarqueeMode(true)
	progress.SetVisible(false)
	progress.SetMinMaxSize(walk.Size{Height: 6}, walk.Size{Height: 6})

	// ---- command bar
	bar := box(mw, hbox(walk.Margins{}, 6))
	filter, _ := walk.NewLineEdit(bar)
	filter.SetCueBanner("Search all columns  (Ctrl+F)")
	filter.SetMinMaxSize(walk.Size{Width: 280, Height: 26}, walk.Size{Width: 280})
	colFilterBtn := button(bar, "Filter column...", "Pick a column to filter. Also: right-click a heading, or use its arrow", 110)
	clearBtn := button(bar, "Clear filters", "Remove every filter on this tab", 90)
	walk.NewVSeparator(bar)
	removeBtn := button(bar, "Remove selected rows", "Delete the selected rows from the data (Ctrl/Shift-click to select several)", 140)
	keepBtn := button(bar, "Keep only filtered rows", "Permanently drop every row hidden by the current filters", 150)
	restoreBtn := button(bar, "Restore all rows", "Bring back the original rows of this tab", 110)
	walk.NewHSpacer(bar)
	xlsxBtn := button(bar, "Export Excel...", "Save every tab to one .xlsx workbook (Ctrl+S)", 110)
	csvBtn := button(bar, "Export CSV...", "Save one .csv file per tab into a folder", 100)
	allBtns := []*walk.PushButton{xlsxBtn, csvBtn, clearBtn, colFilterBtn, removeBtn, keepBtn, restoreBtn}
	for _, b := range allBtns {
		b.SetEnabled(false)
	}
	filter.SetEnabled(false)

	// ---- tabs (bold names) with a welcome page until data arrives
	tabs, _ := walk.NewTabWidget(mw)
	if uiBold != nil {
		tabs.SetFont(uiBold)
	}

	// ---- status bar
	sb := mw.StatusBar()
	msgItem := walk.NewStatusBarItem()
	msgItem.SetWidth(700)
	cntItem := walk.NewStatusBarItem()
	cntItem.SetWidth(320)
	sb.Items().Add(msgItem)
	sb.Items().Add(cntItem)
	sb.SetVisible(true)
	setStatus := func(s string) { msgItem.SetText(s) }
	setStatus("Ready")

	showWelcome := func() {
		page, _ := walk.NewTabPage()
		page.SetTitle("Getting started")
		page.SetLayout(vbox(walk.Margins{HNear: 20, VNear: 20, HFar: 20, VFar: 20}, 8))
		walk.NewVSpacer(page)
		t, _ := walk.NewLabel(page)
		t.SetTextAlignment(walk.AlignCenter)
		t.SetText("No inventory loaded yet")
		if f, _ := walk.NewFont("Segoe UI", 16, walk.FontBold); f != nil {
			t.SetFont(f)
		}
		d, _ := walk.NewLabel(page)
		d.SetTextAlignment(walk.AlignCenter)
		d.SetTextColor(muted)
		d.SetText("Enter your vCenter or ESXi details above and press Collect.\nYou can then sort, filter and tidy every tab before exporting to Excel or CSV.")
		walk.NewVSpacer(page)
		tabs.Pages().Add(page)
	}
	showWelcome()

	var (
		views   []*walk.TableView
		pages   []*walk.TabPage
		cols    [][]*walk.TableViewColumn
		syncing bool
		loading bool
	)
	curIdx := func() int {
		i := tabs.CurrentIndex()
		if i < 0 || i >= len(models) {
			return -1
		}
		return i
	}
	// (re)apply the header arrows and repaint headings (filtered columns are drawn amber)
	decorate := func(i int) {
		markHeaderButtons(views[i], len(cols[i]))
		repaintHeader(views[i])
	}
	refreshCounts := func(i int) {
		m := models[i]
		pages[i].SetTitle(fmt.Sprintf("%s (%d)", m.sheet.Name, len(m.sheet.Rows)))
		cntItem.SetText(fmt.Sprintf("%s: %d of %d rows shown", m.sheet.Name, len(m.view), len(m.sheet.Rows)))
		switch {
		case m.Filtered():
			setStatus("Filters active on this tab. Amber headings show filtered columns.")
		case len(m.sheet.Rows) != len(m.orig):
			setStatus(fmt.Sprintf("%d rows removed from this tab. 'Restore all rows' brings them back.", len(m.orig)-len(m.sheet.Rows)))
		default:
			setStatus("Click a heading to sort. Right-click a heading, or use 'Filter column...', to filter.")
		}
	}
	syncControls := func() {
		if loading {
			return
		}
		i := curIdx()
		if i < 0 {
			return
		}
		syncing = true
		filter.SetText(models[i].query)
		syncing = false
		decorate(i)
		refreshCounts(i)
	}
	filter.TextChanged().Attach(func() {
		if syncing {
			return
		}
		if i := curIdx(); i >= 0 {
			models[i].SetSearch(filter.Text())
			refreshCounts(i)
		}
	})
	tabs.CurrentIndexChanged().Attach(syncControls)
	clearBtn.Clicked().Attach(func() {
		if i := curIdx(); i >= 0 {
			models[i].ClearFilters()
			syncControls()
		}
	})
	openColumnFilter := func(i, col int) {
		if i >= len(models) || col < 0 || col >= len(models[i].sheet.Header) {
			return
		}
		if columnFilterDialog(mw, models[i], col) {
			decorate(i)
			refreshCounts(i)
		}
	}
	openHeaderMenu := func(i, col int, x, y int32) {
		if i >= len(models) || col < 0 || col >= len(models[i].sheet.Header) {
			return
		}
		m := models[i]
		switch headerMenu(mw.Handle(), x, y, m.filters[col] != nil) {
		case 1:
			openColumnFilter(i, col)
		case 2:
			m.SetColumnFilter(col, nil)
		case 3:
			m.Sort(col, walk.SortAscending)
		case 4:
			m.Sort(col, walk.SortDescending)
		case 5:
			m.ClearFilters()
			syncControls()
		}
		decorate(i)
		refreshCounts(i)
	}
	colFilterBtn.Clicked().Attach(func() {
		i := curIdx()
		if i < 0 {
			return
		}
		m := models[i]
		fl := make([]bool, len(m.sheet.Header))
		for c := range fl {
			fl[c] = m.filters[c] != nil
		}
		if c := columnPickMenu(mw.Handle(), m.sheet.Header, fl); c >= 0 {
			openColumnFilter(i, c)
		}
	})
	removeBtn.Clicked().Attach(func() {
		if i := curIdx(); i >= 0 {
			sel := views[i].SelectedIndexes()
			if len(sel) == 0 {
				walk.MsgBox(mw, "Remove rows", "Select one or more rows first (Ctrl/Shift-click for several).", walk.MsgBoxIconInformation)
				return
			}
			models[i].RemoveRows(sel)
			views[i].SetSelectedIndexes(nil)
			refreshCounts(i)
		}
	})
	keepBtn.Clicked().Attach(func() {
		if i := curIdx(); i >= 0 {
			if !models[i].Filtered() {
				walk.MsgBox(mw, "Keep filtered rows", "Apply a filter first (search box or a column filter). Rows that do not match will be removed.", walk.MsgBoxIconInformation)
				return
			}
			models[i].KeepFiltered()
			syncControls()
		}
	})
	restoreBtn.Clicked().Attach(func() {
		if i := curIdx(); i >= 0 {
			models[i].Restore()
			syncControls()
		}
	})

	showSheets := func(ss []*Sheet) {
		// walk panics if a layout pass runs while the tab control has no pages, so the new
		// pages are added first and the old ones removed afterwards.
		loading = true
		defer func() { loading = false }()
		oldN := tabs.Pages().Len()
		if len(ss) == 0 {
			return // keep the current pages: walk panics on an empty tab control
		}
		sheets, models, views, pages, cols = ss, nil, nil, nil, nil
		for _, s := range ss {
			page, _ := walk.NewTabPage()
			page.SetTitle(fmt.Sprintf("%s (%d)", s.Name, len(s.Rows)))
			page.SetLayout(vbox(walk.Margins{HNear: 4, VNear: 4, HFar: 4, VFar: 4}, 0))
			tv, _ := walk.NewTableView(page)
			tv.SetColumnsOrderable(true)
			if uiFont != nil {
				tv.SetFont(uiFont)
			}
			tv.SetMultiSelection(true)
			var tcols []*walk.TableViewColumn
			for ci, h := range s.Header {
				c := walk.NewTableViewColumn()
				c.SetTitle(h)
				c.SetWidth(autoWidth(s, ci))
				tv.Columns().Add(c)
				tcols = append(tcols, c)
			}
			m := newSheetModel(s)
			tv.SetCellStyler(m)
			idx := len(models)
			hookHeader(tv, headerHooks{
				onDropdown: func(col int) { mw.Synchronize(func() { openColumnFilter(idx, col) }) },
				onContext: func(col int, x, y int32) {
					mw.Synchronize(func() { openHeaderMenu(idx, col, x, y) })
				},
				style: func(col int) (string, bool, int) {
					sortDir := 0
					if m.sorted && m.SortedColumn() == col {
						sortDir = 1
						if m.SortOrder() == walk.SortDescending {
							sortDir = 2
						}
					}
					return s.Header[col], m.filters[col] != nil, sortDir
				},
			})
			cols = append(cols, tcols)
			models = append(models, m)
			views = append(views, tv)
			pages = append(pages, page)
			tv.SetModel(m)
			tabs.Pages().Add(page)
		}
		for i := 0; i < oldN; i++ {
			tabs.Pages().RemoveAt(0)
		}
		if len(ss) > 0 {
			tabs.SetCurrentIndex(0)
		}
		for _, b := range allBtns {
			b.SetEnabled(len(ss) > 0)
		}
		filter.SetEnabled(len(ss) > 0)
		loading = false
		syncControls()
	}

	busy := false
	endBusy := func() {
		busy = false
		collectBtn.SetEnabled(true)
		gb.SetEnabled(true)
		progress.SetVisible(false)
	}
	finish := func(ss []*Sheet, results []ConnResult, t0 time.Time) {
		endBusy()
		ok, vms, hosts := 0, 0, 0
		var failed []string
		for _, r := range results {
			if r.OK {
				ok++
				vms += r.VMs
				hosts += r.Hosts
			} else {
				failed = append(failed, r.Label+": "+r.Error)
			}
		}
		if ok == 0 || len(ss) == 0 {
			msg := "Could not collect from any vCenter:\n\n" + strings.Join(failed, "\n")
			setStatus("Failed: " + strings.Join(failed, "; "))
			walk.MsgBox(mw, "Collection failed", msg, walk.MsgBoxIconError)
			return
		}
		showSheets(ss)
		msg := fmt.Sprintf("Collected %d of %d vCenter(s): %d VMs, %d hosts in %s. Review the tabs, then export.",
			ok, len(results), vms, hosts, time.Since(t0).Round(time.Millisecond))
		setStatus(msg)
		if i := curIdx(); i >= 0 {
			cntItem.SetText(fmt.Sprintf("%s: %d of %d rows shown", models[i].sheet.Name, len(models[i].view), len(models[i].sheet.Rows)))
		}
		if len(failed) > 0 {
			walk.MsgBox(mw, "Some vCenters failed", fmt.Sprintf("%d of %d vCenter(s) could not be collected (details are on the vCenters tab):\n\n%s",
				len(failed), len(results), strings.Join(failed, "\n")), walk.MsgBoxIconWarning)
		}
	}
	savePrefs := func() {
		b := mw.BoundsPixels()
		s := settings{Server: strings.TrimSpace(server.Text()), User: strings.TrimSpace(user.Text()), Verify: verify.Checked(), Width: b.Width, Height: b.Height}
		for _, c := range extra {
			s.Extra = append(s.Extra, savedConn{Server: c.Server, User: c.User, Source: c.Source})
		}
		saveSettings(s)
	}
	updateMore := func() {
		if len(extra) == 0 {
			moreBtn.SetText("More vCenters / VCF...")
		} else {
			moreBtn.SetText(fmt.Sprintf("More vCenters / VCF (%d)...", len(extra)))
		}
	}
	updateMore()
	moreBtn.Clicked().Attach(func() {
		connectionsDialog(mw, &extra, !verify.Checked())
		updateMore()
		savePrefs()
	})
	doCollect := func() {
		if busy {
			return
		}
		srv, usr, pw, ins := strings.TrimSpace(server.Text()), strings.TrimSpace(user.Text()), pass.Text(), !verify.Checked()
		var targets []Target
		if srv != "" {
			if usr == "" {
				walk.MsgBox(mw, "Connection", "Please enter the user name.", walk.MsgBoxIconWarning)
				return
			}
			targets = append(targets, Target{Server: srv, User: usr, Password: pw, Source: "Manual", Insecure: ins})
		}
		for _, c := range extra {
			t := Target{Server: c.Server, User: firstNonEmpty(c.User, usr), Password: firstNonEmpty(c.Password, pw), Source: c.Source, Insecure: ins}
			if t.User == "" {
				walk.MsgBox(mw, "Connection", "No user name for "+c.Server+". Enter one in the main window or in the vCenter list.", walk.MsgBoxIconWarning)
				return
			}
			targets = append(targets, t)
		}
		targets = dedupeTargets(targets)
		if len(targets) == 0 {
			walk.MsgBox(mw, "Connection", "Please enter a vCenter or ESXi host (or add some with 'More vCenters / VCF').", walk.MsgBoxIconWarning)
			return
		}
		savePrefs()
		busy = true
		collectBtn.SetEnabled(false)
		gb.SetEnabled(false)
		progress.SetVisible(true)
		if len(targets) == 1 {
			setStatus("Connecting to " + vcLabel(targets[0].Server) + " and collecting...")
		} else {
			setStatus(fmt.Sprintf("Connecting to %d vCenters and collecting in parallel...", len(targets)))
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			t0 := time.Now()
			var ss []*Sheet
			var results []ConnResult
			perr := safely(func() { ss, results = CollectMany(ctx, targets) }) // each vCenter also recovers on its own
			mw.Synchronize(func() {
				// showing the results must not be able to kill the app either
				if perr == nil {
					perr = safely(func() { finish(ss, results, t0) })
				}
				if perr != nil {
					endBusy()
					setStatus(perr.Error())
					walk.MsgBox(mw, "Unexpected error", perr.Error(), walk.MsgBoxIconError)
				}
			})
		}()
	}
	collectBtn.Clicked().Attach(doCollect)
	pass.KeyDown().Attach(func(k walk.Key) {
		if k == walk.KeyReturn {
			doCollect()
		}
	})

	exportXLSX := func() {
		if len(sheets) == 0 {
			return
		}
		dlg := new(walk.FileDialog)
		dlg.Title = "Export to Excel"
		dlg.Filter = "Excel workbook (*.xlsx)|*.xlsx"
		dlg.FilePath = "bcvmtools_" + time.Now().Format("20060102_150405") + ".xlsx"
		if ok, _ := dlg.ShowSave(mw); !ok {
			return
		}
		p := dlg.FilePath
		if !strings.HasSuffix(strings.ToLower(p), ".xlsx") {
			p += ".xlsx"
		}
		if err := WriteXLSX(p, sheets); err != nil {
			walk.MsgBox(mw, "Export failed", err.Error(), walk.MsgBoxIconError)
			return
		}
		setStatus("Exported " + p)
	}
	xlsxBtn.Clicked().Attach(exportXLSX)
	csvBtn.Clicked().Attach(func() {
		dlg := new(walk.FileDialog)
		dlg.Title = "Choose a folder for the CSV files (one file per tab)"
		dlg.Filter = "CSV files (*.csv)|*.csv"
		dlg.FilePath = "inventory.csv"
		if ok, _ := dlg.ShowSave(mw); !ok {
			return
		}
		dir := filepath.Dir(dlg.FilePath)
		if err := WriteCSV(dir, sheets); err != nil {
			walk.MsgBox(mw, "Export failed", err.Error(), walk.MsgBoxIconError)
			return
		}
		setStatus("Exported CSV files to " + dir)
	})

	// ---- keyboard shortcuts
	addKey := func(mod walk.Modifiers, key walk.Key, fn func()) {
		a := walk.NewAction()
		a.SetShortcut(walk.Shortcut{Modifiers: mod, Key: key})
		a.Triggered().Attach(fn)
		mw.ShortcutActions().Add(a)
	}
	addKey(walk.ModControl, walk.KeyF, func() {
		if filter.Enabled() {
			filter.SetFocus()
		}
	})
	addKey(walk.ModControl, walk.KeyS, exportXLSX)
	addKey(0, walk.KeyF5, doCollect)

	// ---- window placement / persistence
	sw, sh := int(win.GetSystemMetrics(win.SM_CXSCREEN)), int(win.GetSystemMetrics(win.SM_CYSCREEN))
	w, h := min(set.Width, sw-40), min(set.Height, sh-80)
	mw.SetBoundsPixels(walk.Rectangle{X: (sw - w) / 2, Y: (sh-h)/2 - 20, Width: w, Height: h})
	mw.Closing().Attach(func(*bool, walk.CloseReason) { savePrefs() })
	if strings.TrimSpace(server.Text()) == "" {
		server.SetFocus()
	} else {
		pass.SetFocus()
	}

	// test hook: BCVMTOOLS_AUTOCOLLECT=1 starts a collection as soon as the window is up
	if os.Getenv("BCVMTOOLS_AUTOCOLLECT") != "" {
		go func() {
			time.Sleep(500 * time.Millisecond)
			mw.Synchronize(doCollect)
		}()
	}
	mw.Show()
	mw.Run()
	return nil
}
