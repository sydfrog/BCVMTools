//go:build windows

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// Conn is one vCenter (or ESXi host) in the connection list, with its own credentials.
// Passwords live in memory only.
type Conn struct {
	Server   string
	User     string
	Password string
	Source   string // "Manual", "VCF Ops: host / name" or "VCF: host / domain"
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
			return "(not set)"
		}
		return c.User
	case 2:
		if c.Password == "" {
			return "(not set)"
		}
		return "••••••••"
	default:
		return c.Source
	}
}

// StyleCell highlights credentials that are still missing so they are not overlooked.
func (m *connModel) StyleCell(st *walk.CellStyle) {
	if safeUI || st.Row() >= len(*m.items) {
		return
	}
	c := (*m.items)[st.Row()]
	if (st.Col() == 1 && c.User == "") || (st.Col() == 2 && c.Password == "") {
		st.BackgroundColor = walk.RGB(255, 236, 190)
	}
}

func hasConn(conns []Conn, server string) bool {
	k := strings.ToLower(vcLabel(server))
	for _, c := range conns {
		if strings.ToLower(vcLabel(c.Server)) == k {
			return true
		}
	}
	return false
}

// vcfDialog discovers the vCenters of a VCF instance (VCF Operations or SDDC Manager) and adds them
// to the main connection list. Credentials for the vCenters can be set here or later, per vCenter
// or for all at once, in the main window. It reports whether anything was added.
func vcfDialog(owner walk.Form, conns *[]Conn, insecure bool) bool {
	dlg, err := walk.NewDialog(owner)
	if err != nil {
		return false
	}
	if uiFont != nil {
		dlg.SetFont(uiFont)
	}
	dlg.SetTitle("Discover vCenters from VMware Cloud Foundation")
	dlg.SetLayout(vbox(walk.Margins{HNear: 12, VNear: 12, HFar: 12, VFar: 12}, 10))
	dlg.SetMinMaxSize(walk.Size{Width: 980, Height: 400}, walk.Size{})
	added := false

	hdr, _ := walk.NewLabel(dlg)
	hdr.SetText("Log in to VCF Operations (VCF 9.x) or SDDC Manager to find every vCenter it manages.\nThe vCenters are added to the main connection list, where each one can have its own user and password.")
	hdr.SetTextColor(muted)

	r0 := box(dlg, hbox(walk.Margins{}, 10))
	mc := box(r0, vbox(walk.Margins{}, 3))
	ml, _ := walk.NewLabel(mc)
	ml.SetText("Discover from")
	ml.SetTextColor(muted)
	mode, _ := walk.NewComboBox(mc)
	mode.SetModel([]string{"VCF Operations  (VCF 9.0 / 9.1)", "SDDC Manager  (VCF 5.x / 9.0)"})
	mode.SetCurrentIndex(0)
	mode.SetMinMaxSize(walk.Size{Width: 300, Height: 24}, walk.Size{Width: 300})
	walk.NewHSpacer(r0)

	r1 := box(dlg, hbox(walk.Margins{}, 10))
	vh := field(r1, "VCF Operations / SDDC Manager host", 280, false, "")
	vu := field(r1, "User", 180, false, "")
	vp := field(r1, "Password", 190, true, "")
	va := field(r1, "Auth source (Operations only; blank = local)", 260, false, "")
	walk.NewHSpacer(r1)

	same, _ := walk.NewCheckBox(dlg)
	same.SetText("Use this same user and password for the discovered vCenters (typical when they share one SSO domain)")
	r2 := box(dlg, hbox(walk.Margins{}, 10))
	cu := field(r2, "vCenter user (optional - can be set later in the list)", 330, false, "")
	cp := field(r2, "vCenter password (optional)", 300, true, "")
	walk.NewHSpacer(r2)
	same.CheckedChanged().Attach(func() {
		cu.SetEnabled(!same.Checked())
		cp.SetEnabled(!same.Checked())
	})
	mode.CurrentIndexChanged().Attach(func() {
		va.SetEnabled(mode.CurrentIndex() == 0)
		same.SetChecked(mode.CurrentIndex() == 1) // SDDC Manager SSO logins usually work on the vCenters too
	})

	r3 := box(dlg, hbox(walk.Margins{}, 10))
	discover := button(r3, "Discover and add", "Log in and add every vCenter it knows about", 150)
	status, _ := walk.NewLabel(r3)
	status.SetTextColor(muted)
	walk.NewHSpacer(r3)
	walk.NewVSpacer(dlg)
	bot := box(dlg, hbox(walk.Margins{}, 8))
	walk.NewHSpacer(bot)
	closeBtn := button(bot, "Close", "", 90)
	dlg.SetDefaultButton(closeBtn)
	dlg.SetCancelButton(closeBtn)
	closeBtn.Clicked().Attach(func() { dlg.Accept() })

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
				n := 0
				for _, v := range vcs {
					if hasConn(*conns, v.FQDN) {
						continue
					}
					*conns = append(*conns, Conn{Server: v.FQDN, User: vcUser, Password: vcPass, Source: tag + host + " / " + v.Domain})
					n++
				}
				added = added || n > 0
				msg := fmt.Sprintf("Found %d vCenter(s), added %d.", len(vcs), n)
				if n > 0 && (vcUser == "" || vcPass == "") {
					msg += " Set their user names and passwords in the main list."
				}
				status.SetText(msg)
			})
		}()
	})

	dlg.Run()
	return added
}
