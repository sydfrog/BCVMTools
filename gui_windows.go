//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
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
	return max(70, min(n, 55)*7+34)
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

func runGUI() error {
	var (
		sheets []*Sheet
		models []*sheetModel
	)
	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	mw.SetTitle("BCVMTools - VMware inventory")
	mw.SetSize(walk.Size{Width: 1280, Height: 780})
	mw.SetLayout(walk.NewVBoxLayout())

	// ---- connection bar
	bar, _ := walk.NewComposite(mw)
	bar.SetLayout(walk.NewHBoxLayout())
	field := func(label string, w int, pw bool, init string) *walk.LineEdit {
		l, _ := walk.NewLabel(bar)
		l.SetText(label)
		e, _ := walk.NewLineEdit(bar)
		e.SetPasswordMode(pw)
		e.SetText(init)
		e.SetMinMaxSize(walk.Size{Width: w}, walk.Size{Width: w})
		return e
	}
	server := field("vCenter / ESXi:", 220, false, os.Getenv("VI_SERVER"))
	user := field("User:", 180, false, os.Getenv("VI_USER"))
	pass := field("Password:", 140, true, os.Getenv("VI_PASSWORD"))
	verify, _ := walk.NewCheckBox(bar)
	verify.SetText("Verify TLS certificate")
	collectBtn, _ := walk.NewPushButton(bar)
	collectBtn.SetText("Collect")
	walk.NewHSpacer(bar)

	// ---- filter bar
	bar2, _ := walk.NewComposite(mw)
	bar2.SetLayout(walk.NewHBoxLayout())
	fl, _ := walk.NewLabel(bar2)
	fl.SetText("Search all columns:")
	filter, _ := walk.NewLineEdit(bar2)
	filter.SetMinMaxSize(walk.Size{Width: 240}, walk.Size{Width: 240})
	clearBtn, _ := walk.NewPushButton(bar2)
	clearBtn.SetText("Clear all filters")
	removeBtn, _ := walk.NewPushButton(bar2)
	removeBtn.SetText("Remove selected rows")
	keepBtn, _ := walk.NewPushButton(bar2)
	keepBtn.SetText("Keep only filtered rows")
	restoreBtn, _ := walk.NewPushButton(bar2)
	restoreBtn.SetText("Restore all rows")
	walk.NewHSpacer(bar2)

	// ---- export bar
	bar3, _ := walk.NewComposite(mw)
	bar3.SetLayout(walk.NewHBoxLayout())
	xlsxBtn, _ := walk.NewPushButton(bar3)
	xlsxBtn.SetText("Export Excel...")
	csvBtn, _ := walk.NewPushButton(bar3)
	csvBtn.SetText("Export CSV folder...")
	walk.NewHSpacer(bar3)
	for _, b := range []*walk.PushButton{xlsxBtn, csvBtn, clearBtn, removeBtn, keepBtn, restoreBtn} {
		b.SetEnabled(false)
	}

	tabs, _ := walk.NewTabWidget(mw)
	status, _ := walk.NewLabel(mw)
	status.SetText("Enter connection details and press Collect.")

	var (
		views   []*walk.TableView
		pages   []*walk.TabPage
		cols    [][]*walk.TableViewColumn
		syncing bool
	)
	curIdx := func() int {
		i := tabs.CurrentIndex()
		if i < 0 || i >= len(models) {
			return -1
		}
		return i
	}
	// headings show a dot when the column has an active filter; the split-button arrow is (re)applied
	decorate := func(i int) {
		m := models[i]
		for c, col := range cols[i] {
			t := m.sheet.Header[c]
			if m.filters[c] != nil {
				t += "  \u25CF"
			}
			col.SetTitle(t)
		}
		markHeaderButtons(views[i], len(cols[i]))
	}
	refreshCounts := func(i int) {
		m := models[i]
		pages[i].SetTitle(fmt.Sprintf("%s (%d)", m.sheet.Name, len(m.sheet.Rows)))
		status.SetText(fmt.Sprintf("%s: showing %d of %d rows (original %d). Click a heading to sort; use its arrow to filter.",
			m.sheet.Name, len(m.view), len(m.sheet.Rows), len(m.orig)))
	}
	syncControls := func() {
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
		if col < 0 || col >= len(models[i].sheet.Header) {
			return
		}
		if columnFilterDialog(mw, models[i], col) {
			decorate(i)
			refreshCounts(i)
		}
	}
	removeBtn.Clicked().Attach(func() {
		if i := curIdx(); i >= 0 {
			sel := views[i].SelectedIndexes()
			if len(sel) == 0 {
				walk.MsgBox(mw, "BCVMTools", "Select one or more rows first (Ctrl/Shift-click for several).", walk.MsgBoxIconInformation)
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
				walk.MsgBox(mw, "BCVMTools", "Apply a filter first (search box or a heading arrow); rows that do not match will be removed.", walk.MsgBoxIconInformation)
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
		tabs.Pages().Clear()
		sheets, models, views, pages, cols = ss, nil, nil, nil, nil
		for _, s := range ss {
			page, _ := walk.NewTabPage()
			page.SetTitle(fmt.Sprintf("%s (%d)", s.Name, len(s.Rows)))
			page.SetLayout(walk.NewVBoxLayout())
			tv, _ := walk.NewTableView(page)
			tv.SetColumnsOrderable(true)
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
			hookHeaderDropdown(tv, func(col int) {
				mw.Synchronize(func() { openColumnFilter(idx, col) })
			})
			cols = append(cols, tcols)
			models = append(models, m)
			views = append(views, tv)
			pages = append(pages, page)
			tv.SetModel(m)
			tabs.Pages().Add(page)
		}
		for _, b := range []*walk.PushButton{xlsxBtn, csvBtn, clearBtn, removeBtn, keepBtn, restoreBtn} {
			b.SetEnabled(len(ss) > 0)
		}
		syncControls()
	}

	collectBtn.Clicked().Attach(func() {
		if server.Text() == "" || user.Text() == "" {
			walk.MsgBox(mw, "BCVMTools", "Server and user are required.", walk.MsgBoxIconWarning)
			return
		}
		srv, usr, pw, ins := server.Text(), user.Text(), pass.Text(), !verify.Checked()
		collectBtn.SetEnabled(false)
		status.SetText("Connecting and collecting...")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			t0 := time.Now()
			inv, ss, _, _, err := CollectAll(ctx, srv, usr, pw, ins)
			mw.Synchronize(func() {
				collectBtn.SetEnabled(true)
				if err != nil {
					status.SetText("Failed: " + err.Error())
					walk.MsgBox(mw, "BCVMTools", err.Error(), walk.MsgBoxIconError)
					return
				}
				showSheets(ss)
				msg := fmt.Sprintf("%d VMs, %d hosts, %d datastores collected in %s. Review the tabs, then export.",
					len(inv.VMs), len(inv.Hosts), len(inv.DS), time.Since(t0).Round(time.Millisecond))
				if len(inv.Warnings) > 0 {
					msg += fmt.Sprintf(" (%d warnings: %s)", len(inv.Warnings), inv.Warnings[0])
				}
				status.SetText(msg)
			})
		}()
	})

	stamp := time.Now().Format("20060102_150405")
	xlsxBtn.Clicked().Attach(func() {
		dlg := new(walk.FileDialog)
		dlg.Title = "Export to Excel"
		dlg.Filter = "Excel workbook (*.xlsx)|*.xlsx"
		dlg.FilePath = "bcvmtools_" + stamp + ".xlsx"
		if ok, _ := dlg.ShowSave(mw); !ok {
			return
		}
		p := dlg.FilePath
		if !strings.HasSuffix(strings.ToLower(p), ".xlsx") {
			p += ".xlsx"
		}
		if err := WriteXLSX(p, sheets); err != nil {
			walk.MsgBox(mw, "BCVMTools", err.Error(), walk.MsgBoxIconError)
			return
		}
		status.SetText("Exported " + p)
	})
	csvBtn.Clicked().Attach(func() {
		dlg := new(walk.FileDialog)
		dlg.Title = "Choose a folder for the CSV files"
		dlg.Filter = "CSV files (*.csv)|*.csv"
		dlg.FilePath = "inventory.csv"
		if ok, _ := dlg.ShowSave(mw); !ok {
			return
		}
		dir := filepath.Dir(dlg.FilePath)
		if err := WriteCSV(dir, sheets); err != nil {
			walk.MsgBox(mw, "BCVMTools", err.Error(), walk.MsgBoxIconError)
			return
		}
		status.SetText("Exported CSV files to " + dir)
	})

	mw.Show()
	mw.Run()
	return nil
}
