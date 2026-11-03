//go:build windows

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// Conn is one additional vCenter held by the GUI. Password stays in memory only.
type Conn struct {
	Server   string
	User     string
	Password string
	Source   string
}

type connModel struct {
	walk.TableModelBase
	items *[]Conn
}

func (m *connModel) RowCount() int { return len(*m.items) }
func (m *connModel) Value(row, col int) interface{} {
	c := (*m.items)[row]
	switch col {
	case 0:
		return c.Server
	case 1:
		if c.User == "" {
			return "(main window user)"
		}
		return c.User
	default:
		return c.Source
	}
}

// connectionsDialog lets the user add more vCenters, or discover them from a VCF SDDC Manager.
// It edits *conns in place.
func connectionsDialog(owner walk.Form, conns *[]Conn, insecure bool) {
	dlg, err := walk.NewDialog(owner)
	if err != nil {
		return
	}
	if uiFont != nil {
		dlg.SetFont(uiFont)
	}
	dlg.SetTitle("More vCenters / VCF")
	dlg.SetLayout(vbox(walk.Margins{HNear: 12, VNear: 12, HFar: 12, VFar: 12}, 8))
	dlg.SetMinMaxSize(walk.Size{Width: 820, Height: 640}, walk.Size{})

	hdr, _ := walk.NewLabel(dlg)
	hdr.SetText("Additional vCenters. These are collected at the same time as the vCenter in the main window,\nand every row is tagged with the vCenter it came from.")
	hdr.SetTextColor(muted)

	model := &connModel{items: conns}
	tv, _ := walk.NewTableView(dlg)
	tv.SetMultiSelection(true)
	for _, c := range []struct {
		t string
		w int
	}{{"vCenter", 280}, {"User", 200}, {"Source", 260}} {
		col := walk.NewTableViewColumn()
		col.SetTitle(c.t)
		col.SetWidth(c.w)
		tv.Columns().Add(col)
	}
	tv.SetModel(model)
	tv.SetMinMaxSize(walk.Size{Height: 150}, walk.Size{})

	// ---- add a single vCenter
	gb1, _ := walk.NewGroupBox(dlg)
	gb1.SetTitle("Add a vCenter")
	gb1.SetLayout(hbox(walk.Margins{HNear: 10, VNear: 8, HFar: 10, VFar: 10}, 10))
	srv := field(gb1, "vCenter or ESXi host", 240, false, "")
	usr := field(gb1, "User (blank = main window user)", 200, false, "")
	pw := field(gb1, "Password (blank = main window password)", 220, true, "")
	b1 := box(gb1, vbox(walk.Margins{}, 3))
	sp, _ := walk.NewLabel(b1)
	sp.SetText(" ")
	add := button(b1, "Add", "Add this vCenter to the list", 80)
	walk.NewHSpacer(gb1)

	// ---- VCF discovery
	gb2, _ := walk.NewGroupBox(dlg)
	gb2.SetTitle("VMware Cloud Foundation: discover vCenters from SDDC Manager")
	gb2.SetLayout(vbox(walk.Margins{HNear: 10, VNear: 8, HFar: 10, VFar: 10}, 8))
	r1 := box(gb2, hbox(walk.Margins{}, 10))
	vh := field(r1, "SDDC Manager host", 240, false, "")
	vu := field(r1, "SDDC Manager user", 200, false, "")
	vp := field(r1, "SDDC Manager password", 220, true, "")
	walk.NewHSpacer(r1)
	same, _ := walk.NewCheckBox(gb2)
	same.SetText("Use the same user and password for the discovered vCenters (typical when they share one SSO domain)")
	same.SetChecked(true)
	r2 := box(gb2, hbox(walk.Margins{}, 10))
	cu := field(r2, "vCenter user", 240, false, "")
	cp := field(r2, "vCenter password", 220, true, "")
	cu.SetEnabled(false)
	cp.SetEnabled(false)
	walk.NewHSpacer(r2)
	same.CheckedChanged().Attach(func() {
		cu.SetEnabled(!same.Checked())
		cp.SetEnabled(!same.Checked())
	})
	r3 := box(gb2, hbox(walk.Margins{}, 10))
	discover := button(r3, "Discover and add", "Log in to SDDC Manager and add every vCenter it manages", 150)
	status, _ := walk.NewLabel(r3)
	status.SetTextColor(muted)
	walk.NewHSpacer(r3)

	// ---- bottom buttons
	bot := box(dlg, hbox(walk.Margins{}, 8))
	remove := button(bot, "Remove selected", "Remove the selected vCenters from the list", 120)
	clearAll := button(bot, "Remove all", "Empty the list", 90)
	walk.NewHSpacer(bot)
	closeBtn := button(bot, "Close", "", 90)
	dlg.SetDefaultButton(closeBtn)
	dlg.SetCancelButton(closeBtn)
	closeBtn.Clicked().Attach(func() { dlg.Accept() })

	exists := func(server string) bool {
		k := strings.ToLower(vcLabel(server))
		for _, c := range *conns {
			if strings.ToLower(vcLabel(c.Server)) == k {
				return true
			}
		}
		return false
	}
	changed := func() { model.PublishRowsReset() }

	add.Clicked().Attach(func() {
		s := strings.TrimSpace(srv.Text())
		if s == "" {
			walk.MsgBox(dlg, "Add vCenter", "Please enter the vCenter or ESXi host name.", walk.MsgBoxIconInformation)
			return
		}
		if exists(s) {
			status.SetText(s + " is already in the list.")
			return
		}
		*conns = append(*conns, Conn{Server: s, User: strings.TrimSpace(usr.Text()), Password: pw.Text(), Source: "Manual"})
		srv.SetText("")
		pw.SetText("")
		changed()
	})
	remove.Clicked().Attach(func() {
		drop := map[int]bool{}
		for _, i := range tv.SelectedIndexes() {
			drop[i] = true
		}
		var keep []Conn
		for i, c := range *conns {
			if !drop[i] {
				keep = append(keep, c)
			}
		}
		*conns = keep
		tv.SetSelectedIndexes(nil)
		changed()
	})
	clearAll.Clicked().Attach(func() { *conns = nil; changed() })

	discover.Clicked().Attach(func() {
		host, user, pass := strings.TrimSpace(vh.Text()), strings.TrimSpace(vu.Text()), vp.Text()
		if host == "" || user == "" {
			walk.MsgBox(dlg, "VCF", "Enter the SDDC Manager host and user.", walk.MsgBoxIconInformation)
			return
		}
		vcUser, vcPass := user, pass
		if !same.Checked() {
			vcUser, vcPass = strings.TrimSpace(cu.Text()), cp.Text()
		}
		discover.SetEnabled(false)
		status.SetText("Contacting SDDC Manager...")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			vcs, err := DiscoverVCF(ctx, host, user, pass, insecure)
			dlg.Synchronize(func() {
				discover.SetEnabled(true)
				if err != nil {
					status.SetText("Failed")
					walk.MsgBox(dlg, "VCF discovery failed", err.Error(), walk.MsgBoxIconError)
					return
				}
				added := 0
				for _, v := range vcs {
					if exists(v.FQDN) {
						continue
					}
					*conns = append(*conns, Conn{Server: v.FQDN, User: vcUser, Password: vcPass, Source: "VCF: " + host + " / " + v.Domain})
					added++
				}
				status.SetText(fmt.Sprintf("Found %d vCenter(s), added %d.", len(vcs), added))
				changed()
			})
		}()
	})

	dlg.Run()
}
