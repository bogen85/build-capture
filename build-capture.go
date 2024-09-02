package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"os/exec"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type buildCapture struct {
	app        *tview.Application
	cmd        *exec.Cmd
	lines      []string
	list       *tview.List
	status     *tview.TextView
	itemCount  *tview.TextView
	errorCount *tview.TextView
	caption    *tview.TextView
	flex       *tview.Flex
	killChan   chan struct{}
	update     func()
	changed    func(idx int, _mt string, _st string, _sc rune)
	errors     int
}

func newBuildCapture(command string, args []string) *buildCapture {
	bc := buildCapture{
		app:        tview.NewApplication(),
		cmd:        exec.Command(command, args...),
		list:       tview.NewList().AddItem(" EOT │ ", "", '\000', nil).ShowSecondaryText(false).SetWrapAround(false),
		status:     tview.NewTextView().SetDynamicColors(true),
		itemCount:  tview.NewTextView().SetDynamicColors(true),
		errorCount: tview.NewTextView().SetDynamicColors(true),
		caption:    tview.NewTextView().SetLabel("(runing) Press 'k' to kill command"),
		flex:       tview.NewFlex(),
		killChan:   make(chan struct{}),
		errors:     0,
	}

	bc.changed = func(idx int, _mt string, _st string, _sc rune) {
		at_end := bc.atBottom()
		n := bc.list.GetItemCount() - 1
		st := func() string {
			if at_end {
				return fmt.Sprintf("%d", n)
			}
			return fmt.Sprintf("%d:%d", idx+1, n)
		}
		bc.itemCount.SetText(fmt.Sprintf("[green]([white]%s[green])[white]", st()))
	}

	bc.update = func() {
		if !bc.atBottom() {
			txt := bc.lines[bc.list.GetCurrentItem()]
			bc.status.SetText(fmt.Sprintf("[blue]%s", txt))
		}
	}

	bc.list.SetChangedFunc(bc.changed)

	captionWidth := utf8.RuneCountInString(bc.caption.GetLabel())
	info := tview.NewFlex().AddItem(bc.caption, captionWidth+1, 1, false).
		AddItem(bc.itemCount, 0, 1, false).
		AddItem(bc.errorCount, 0, 1, false)

	bc.flex.SetDirection(tview.FlexRow).
		AddItem(bc.list, 0, 1, true).
		AddItem(info, 1, 1, false).
		AddItem(bc.status, 1, 1, false)

	return &bc
}

func (bc *buildCapture) append0(isError bool, format string, args ...interface{}) {
	bar := func() string {
		if isError {
			return " ╪ "
		}
		return " │ "
	}()

	at_end := bc.atBottom()
	line := fmt.Sprintf(format, args...)
	bc.lines = append(bc.lines, line)
	bc.list.InsertItem(-2, fmt.Sprintf("%4d%s%s", len(bc.lines), bar, line), "", '\000', bc.update)

	if at_end {
		bc.list.SetCurrentItem(-1)
	}

	bc.changed(bc.list.GetCurrentItem(), "", "", '\000')
	bc.app.Draw()
}

func (bc *buildCapture) append(format string, args ...interface{}) {
	bc.append0(false, format, args...)
}

func (bc *buildCapture) appendError(format string, args ...interface{}) {
	bc.errors++
	bc.errorCount.SetText(fmt.Sprintf("[red]([yellow]%d[red])[white]", bc.errors))
	bc.append0(true, format, args...)
}

func (bc *buildCapture) atBottom() bool {
	return bc.list.GetCurrentItem() == (bc.list.GetItemCount() - 1)
}

func (bc *buildCapture) waitForCommand() <-chan error {
	done := make(chan error)
	go func() {
		done <- bc.cmd.Wait()
	}()
	return done

}

func (bc *buildCapture) run() {
	if err := bc.app.SetRoot(bc.flex, true).SetFocus(bc.list).EnableMouse(false).Run(); err != nil {
		log.Fatal(err)
	}
}

func (bc *buildCapture) start() int {

	chPrimary := make(chan struct{})
	chStderr := make(chan struct{})
	chStdout := make(chan struct{})

	var stdout io.ReadCloser
	var stderr io.ReadCloser

	if pipe, err := bc.cmd.StdoutPipe(); err != nil {
		bc.appendError("Error creating StdoutPipe: %s", err)
		return -2
	} else {
		stdout = pipe
	}

	if pipe, err := bc.cmd.StderrPipe(); err != nil {
		bc.appendError("Error creating StderrPipe: %s", err)
		return -2
	} else {
		stderr = pipe
	}

	if err := bc.cmd.Start(); err != nil {
		bc.appendError("Error starting cmd: %s", err)
		return -2
	}

	scannerOut := bufio.NewScanner(stdout)
	scannerErr := bufio.NewScanner(stderr)

	go func() {
		for scannerOut.Scan() {
			bc.append("%s", scannerOut.Text())
		}
		chStdout <- struct{}{}

	}()
	go func() {
		for scannerErr.Scan() {
			bc.appendError("%s", scannerErr.Text())
		}
		chStderr <- struct{}{}
	}()

	primary := func() bool {
		select {
		case <-bc.killChan:
			bc.appendError("Process was killed by user")
			if err := bc.cmd.Process.Kill(); err != nil {
				bc.appendError("Error killing process: %s", err)
				return true
			}
			bc.cmd.Wait()
			return true
		case <-bc.waitForCommand():
			return false
		}
		return false
	}
	go func() {
		killed := primary()
		if !killed && !bc.cmd.ProcessState.Success() {
			bc.appendError("Command failed: rc = %d", bc.cmd.ProcessState.ExitCode())
		}
		chPrimary <- struct{}{}
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-chPrimary:
		case <-chStdout:
		case <-chStderr:
		}
	}
	return bc.cmd.ProcessState.ExitCode()
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	bc := newBuildCapture(os.Args[1], os.Args[2:])

	clear := false
	quit := func() {
		bc.app.Stop()
		if !clear {
			fmt.Println(strings.Join(bc.lines, "\n"))
		}
	}

	finished := false
	killed := false
	bc.list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyRune {
			ch := event.Rune()
			switch ch {
			case 'k':
				if !killed {
					bc.killChan <- struct{}{}
					killed = true
				}
				return nil
			case 'c':
				clear = true
				if finished {
					quit()
				}
				return nil
			case 'q':
				if finished {
					quit()
				}
				return nil
			case '\000':
				return nil
			}
		}
		return event
	})

	go func() {
		rc := bc.start()
		finished = true
		bc.caption.SetLabel("(completed) Press 'q' to exit")
		if rc != 0 {
			bc.appendError("Fail: %s %s", bc.cmd.Path, strings.Join(bc.cmd.Args, " "))
		}
		bc.app.Draw()
	}()

	bc.run()
}
