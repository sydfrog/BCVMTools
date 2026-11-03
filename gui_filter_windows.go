//go:build windows

package main

import (
	"sort"
	"strconv"
	"strings"

	"github.com/lxn/walk"
)

type filterItem struct {
	val     string
	count   int
	checked bool
}

// valueList is the checkbox list inside the filter dialog (shows the items matching the search box).
type valueList struct {
	walk.TableModelBase
	all     []*filterItem
	visible []*filterItem
}

func (v *valueList) RowCount() int { return len(v.visible) }
func (v *valueList) Value(row, col int) interface{} {
	it := v.visible[row]
	if col == 1 {
		return it.count
	}
	if it.val == "" {
		return "(Blanks)"
	}
	return it.val
}
func (v *valueList) Checked(row int) bool { return v.visible[row].checked }
func (v *valueList) SetChecked(row int, c bool) error {
	v.visible[row].checked = c
	return nil
}
func (v *valueList) search(q string) {
	q = strings.ToLower(q)
	v.visible = v.visible[:0]
	for _, it := range v.all {
		if q == "" || strings.Contains(strings.ToLower(it.val), q) || (it.val == "" && strings.Contains("(blanks)", q)) {
			v.visible = append(v.visible, it)
		}
	}
	v.PublishRowsReset()
}

func naturalLess(a, b string) bool {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		return fa < fb
	}
	return strings.ToLower(a) < strings.ToLower(b)
}

// columnFilterDialog shows an Excel-style filter popup for one column.
// It returns true if the model's filter or sort changed.
func columnFilterDialog(owner walk.Form, m *sheetModel, col int) bool {
	counts := map[string]int{}
	for _, r := range m.sheet.Rows {
		counts[cellStr(r, col)]++
	}
	cur := m.filters[col]
	vl := &valueList{}
	for v, n := range counts {
		vl.all = append(vl.all, &filterItem{val: v, count: n, checked: cur == nil || cur[v]})
	}
	sort.Slice(vl.all, func(i, j int) bool { return naturalLess(vl.all[i].val, vl.all[j].val) })
	vl.search("")

	changed := false
	dlg, err := walk.NewDialog(owner)
	if err != nil {
		return false
	}
	dlg.SetTitle("Filter: " + m.sheet.Header[col])
	dlg.SetLayout(walk.NewVBoxLayout())
	dlg.SetMinMaxSize(walk.Size{Width: 360, Height: 520}, walk.Size{})

	sortBar, _ := walk.NewComposite(dlg)
	sortBar.SetLayout(walk.NewHBoxLayout())
	asc, _ := walk.NewPushButton(sortBar)
	asc.SetText("Sort A to Z")
	desc, _ := walk.NewPushButton(sortBar)
	desc.SetText("Sort Z to A")
	asc.Clicked().Attach(func() { m.Sort(col, walk.SortAscending); changed = true; dlg.Accept() })
	desc.Clicked().Attach(func() { m.Sort(col, walk.SortDescending); changed = true; dlg.Accept() })

	search, _ := walk.NewLineEdit(dlg)
	search.SetCueBanner("Search values")
	selAll, _ := walk.NewCheckBox(dlg)
	selAll.SetText("(Select all shown)")
	selAll.SetChecked(true)

	tv, _ := walk.NewTableView(dlg)
	tv.SetCheckBoxes(true)
	tv.SetHeaderHidden(true)
	c0 := walk.NewTableViewColumn()
	c0.SetWidth(230)
	tv.Columns().Add(c0)
	c1 := walk.NewTableViewColumn()
	c1.SetWidth(60)
	tv.Columns().Add(c1)
	tv.SetModel(vl)

	search.TextChanged().Attach(func() {
		vl.search(search.Text())
		all := true
		for _, it := range vl.visible {
			all = all && it.checked
		}
		selAll.SetChecked(all)
	})
	selAll.CheckedChanged().Attach(func() {
		for _, it := range vl.visible {
			it.checked = selAll.Checked()
		}
		vl.PublishRowsReset()
	})

	btns, _ := walk.NewComposite(dlg)
	btns.SetLayout(walk.NewHBoxLayout())
	clear, _ := walk.NewPushButton(btns)
	clear.SetText("Clear filter")
	walk.NewHSpacer(btns)
	ok, _ := walk.NewPushButton(btns)
	ok.SetText("OK")
	cancel, _ := walk.NewPushButton(btns)
	cancel.SetText("Cancel")
	dlg.SetDefaultButton(ok)
	dlg.SetCancelButton(cancel)
	cancel.Clicked().Attach(func() { dlg.Cancel() })
	clear.Clicked().Attach(func() {
		m.SetColumnFilter(col, nil)
		changed = true
		dlg.Accept()
	})
	ok.Clicked().Attach(func() {
		allowed := map[string]bool{}
		every := true
		for _, it := range vl.all {
			if it.checked {
				allowed[it.val] = true
			} else {
				every = false
			}
		}
		if every {
			m.SetColumnFilter(col, nil)
		} else {
			m.SetColumnFilter(col, allowed)
		}
		changed = true
		dlg.Accept()
	})
	dlg.Run()
	return changed
}
