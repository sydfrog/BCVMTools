//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// sheetModel is a virtual, filterable table model over a Sheet (cheap even for 100k rows).
type sheetModel struct {
	walk.TableModelBase
	sheet *Sheet
	view  []int // indexes of visible rows
}

func newSheetModel(s *Sheet) *sheetModel {
	m := &sheetModel{sheet: s}
	m.Filter("")
	return m
}

func (m *sheetModel) RowCount() int { return len(m.view) }

func (m *sheetModel) Value(row, col int) interface{} {
	r := m.sheet.Rows[m.view[row]]
	if col < len(r) && r[col] != nil {
		return fmt.Sprint(r[col])
	}
	return ""
}

func (m *sheetModel) Filter(q string) {
	q = strings.ToLower(q)
	m.view = m.view[:0]
	for i, r := range m.sheet.Rows {
		if q == "" {
			m.view = append(m.view, i)
			continue
		}
		for _, v := range r {
			if v != nil && strings.Contains(strings.ToLower(fmt.Sprint(v)), q) {
				m.view = append(m.view, i)
				break
			}
		}
	}
	m.PublishRowsReset()
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

	// ---- filter + export bar
	bar2, _ := walk.NewComposite(mw)
	bar2.SetLayout(walk.NewHBoxLayout())
	fl, _ := walk.NewLabel(bar2)
	fl.SetText("Filter (current tab):")
	filter, _ := walk.NewLineEdit(bar2)
	filter.SetMinMaxSize(walk.Size{Width: 260}, walk.Size{Width: 260})
	xlsxBtn, _ := walk.NewPushButton(bar2)
	xlsxBtn.SetText("Export Excel...")
	csvBtn, _ := walk.NewPushButton(bar2)
	csvBtn.SetText("Export CSV folder...")
	walk.NewHSpacer(bar2)
	xlsxBtn.SetEnabled(false)
	csvBtn.SetEnabled(false)

	tabs, _ := walk.NewTabWidget(mw)
	status, _ := walk.NewLabel(mw)
	status.SetText("Enter connection details and press Collect.")

	curModel := func() *sheetModel {
		i := tabs.CurrentIndex()
		if i < 0 || i >= len(models) {
			return nil
		}
		return models[i]
	}
	filter.TextChanged().Attach(func() {
		if m := curModel(); m != nil {
			m.Filter(filter.Text())
		}
	})
	tabs.CurrentIndexChanged().Attach(func() {
		if m := curModel(); m != nil {
			m.Filter(filter.Text())
		}
	})

	showSheets := func(ss []*Sheet) {
		tabs.Pages().Clear()
		sheets, models = ss, nil
		for _, s := range ss {
			page, _ := walk.NewTabPage()
			page.SetTitle(fmt.Sprintf("%s (%d)", s.Name, len(s.Rows)))
			page.SetLayout(walk.NewVBoxLayout())
			tv, _ := walk.NewTableView(page)
			tv.SetColumnsOrderable(true)
			for _, h := range s.Header {
				c := walk.NewTableViewColumn()
				c.SetTitle(h)
				c.SetWidth(max(80, 9*len(h)))
				tv.Columns().Add(c)
			}
			m := newSheetModel(s)
			models = append(models, m)
			tv.SetModel(m)
			tabs.Pages().Add(page)
		}
		xlsxBtn.SetEnabled(len(ss) > 0)
		csvBtn.SetEnabled(len(ss) > 0)
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
