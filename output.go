package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/xuri/excelize/v2"
)

// WriteXLSX writes all sheets into one workbook using the streaming writer.
func WriteXLSX(path string, sheets []*Sheet) error {
	f := excelize.NewFile()
	defer f.Close()
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	first := true
	for _, s := range sheets {
		if first {
			f.SetSheetName("Sheet1", s.Name)
			first = false
		} else if _, err := f.NewSheet(s.Name); err != nil {
			return err
		}
		sw, err := f.NewStreamWriter(s.Name)
		if err != nil {
			return err
		}
		hdr := make([]any, len(s.Header))
		for i, h := range s.Header {
			hdr[i] = excelize.Cell{Value: h, StyleID: bold}
		}
		if err := sw.SetRow("A1", hdr); err != nil {
			return err
		}
		for i, r := range s.Rows {
			cell, _ := excelize.CoordinatesToCellName(1, i+2)
			if err := sw.SetRow(cell, r); err != nil {
				return err
			}
		}
		if err := sw.Flush(); err != nil {
			return err
		}
	}
	return f.SaveAs(path)
}

// WriteCSV writes one CSV per sheet into dir, in parallel.
func WriteCSV(dir string, sheets []*Sheet) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(sheets))
	for _, s := range sheets {
		wg.Add(1)
		go func(s *Sheet) {
			defer wg.Done()
			f, err := os.Create(filepath.Join(dir, s.Name+".csv"))
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			w := csv.NewWriter(f)
			w.Write(s.Header)
			rec := make([]string, len(s.Header))
			for _, r := range s.Rows {
				for i := range rec {
					rec[i] = ""
					if i < len(r) && r[i] != nil {
						rec[i] = fmt.Sprint(r[i])
					}
				}
				w.Write(rec)
			}
			w.Flush()
			errs <- w.Error()
		}(s)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
