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
	dlg.SetMinMaxSize(walk.Size{Width: 1000, Height: 700}, walk.Size{})

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
	usr := field(gb1, "User (blank = main window user)", 240, false, "")
	pw := field(gb1, "Password (blank = main window password)", 260, true, "")
	b1 := box(gb1, vbox(walk.Margins{}, 3))
	sp, _ := walk.NewLabel(b1)
	sp.SetText(" ")
	add := button(b1, "Add", "Add this vCenter to the list", 80)
	walk.NewHSpacer(gb1)

	// ---- VCF discovery
	gb2, _ := walk.NewGroupBox(dlg)
	gb2.SetTitle("VMware Cloud Foundation: discover the vCenters automatically")
	gb2.SetLayout(vbox(walk.Margins{HNear: 10, VNear: 8, HFar: 10, VFar: 10}, 8))
	r0 := box(gb2, hbox(walk.Margins{}, 10))
	mc := box(r0, vbox(walk.Margins{}, 3))
	ml, _ := walk.NewLabel(mc)
	ml.SetText("Discover from")
	ml.SetTextColor(muted)
	mode, _ := walk.NewComboBox(mc)
	mode.SetModel([]string{"VCF Operations  (VCF 9.0 / 9.1)", "SDDC Manager  (VCF 5.x / 9.0)"})
	mode.SetCurrentIndex(0)
	mode.SetMinMaxSize(walk.Size{Width: 300, Height: 24}, walk.Size{Width: 300})
	walk.NewHSpacer(r0)
	r1 := box(gb2, hbox(walk.Margins{}, 10))
	vh := field(r1, "VCF Operations / SDDC Manager host", 280, false, "")
	vu := field(r1, "User", 180, false, "")
	vp := field(r1, "Password", 190, true, "")
	va := field(r1, "Auth source (Operations only; blank = local)", 260, false, "")
	walk.NewHSpacer(r1)
	same, _ := walk.NewCheckBox(gb2)
	same.SetText("Use the same user and password to log in to the discovered vCenters")
	r2 := box(gb2, hbox(walk.Margins{}, 10))
	cu := field(r2, "vCenter user (blank = main window user)", 330, false, "")
	cp := field(r2, "vCenter password (blank = main window password)", 350, true, "")
	walk.NewHSpacer(r2)
	same.CheckedChanged().Attach(func() {
		cu.SetEnabled(!same.Checked())
		cp.SetEnabled(!same.Checked())
	})
	mode.CurrentIndexChanged().Attach(func() {
		va.SetEnabled(mode.CurrentIndex() == 0)
		same.SetChecked(mode.CurrentIndex() == 1) // SDDC Manager SSO users usually work on the vCenters too
	})
	r3 := box(gb2, hbox(walk.Margins{}, 10))
	discover := button(r3, "Discover and add", "Log in and add every vCenter it knows about", 150)
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
		host, user, pass, auth := strings.TrimSpace(vh.Text()), strings.TrimSpace(vu.Text()), vp.Text(), strings.TrimSpace(va.Text())
		if host == "" || user == "" {
			walk.MsgBox(dlg, "VCF", "Enter the host and user for VCF Operations / SDDC Manager.", walk.MsgBoxIconInformation)
			return
		}
		ops := mode.CurrentIndex() == 0
		vcUser, vcPass := strings.TrimSpace(cu.Text()), cp.Text()
		if same.Checked() {
			vcUser, vcPass = user, pass
		}
		discover.SetEnabled(false)
		status.SetText("Contacting " + host + "...")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			var vcs []VCFVCenter
			var err error
			if ops {
				vcs, err = DiscoverVCFOps(ctx, host, user, pass, auth, insecure)
			} else {
				vcs, err = DiscoverVCF(ctx, host, user, pass, insecure)
			}
			dlg.Synchronize(func() {
				discover.SetEnabled(true)
				if err != nil {
					status.SetText("Failed")
					walk.MsgBox(dlg, "VCF discovery failed", err.Error(), walk.MsgBoxIconError)
					return
				}
				tag := "VCF: "
				if ops {
					tag = "VCF Ops: "
				}
				added := 0
				for _, v := range vcs {
					if exists(v.FQDN) {
						continue
					}
					*conns = append(*conns, Conn{Server: v.FQDN, User: vcUser, Password: vcPass, Source: tag + host + " / " + v.Domain})
					added++
				}
				status.SetText(fmt.Sprintf("Found %d vCenter(s), added %d.", len(vcs), added))
				changed()
			})
		}()
	})

	dlg.Run()
}
