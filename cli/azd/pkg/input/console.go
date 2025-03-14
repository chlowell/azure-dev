// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package input

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/azure/azure-dev/cli/azd/internal/tracing"
	"github.com/azure/azure-dev/cli/azd/internal/tracing/resource"
	"github.com/azure/azure-dev/cli/azd/pkg/alpha"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/azure/azure-dev/cli/azd/pkg/output/ux"
	"github.com/nathan-fiscaletti/consolesize-go"
	"github.com/theckman/yacspin"
	"go.uber.org/atomic"
)

type SpinnerUxType int

const (
	Step SpinnerUxType = iota
	StepDone
	StepFailed
	StepWarning
	StepSkipped
)

// A shim to allow a single Console construction in the application.
// To be removed once formatter and Console's responsibilities are reconciled
type ConsoleShim interface {
	// True if the console was instantiated with no format options.
	IsUnformatted() bool

	// Gets the underlying formatter used by the console
	GetFormatter() output.Formatter
}

// ShowPreviewerOptions provide the settings to start a console previewer.
type ShowPreviewerOptions struct {
	Prefix       string
	MaxLineCount int
	Title        string
}

type Console interface {
	// Prints out a message to the underlying console write
	Message(ctx context.Context, message string)
	// Prints out a message following a contract ux item
	MessageUxItem(ctx context.Context, item ux.UxItem)
	WarnForFeature(ctx context.Context, id alpha.FeatureId)
	// Prints progress spinner with the given title.
	// If a previous spinner is running, the title is updated.
	ShowSpinner(ctx context.Context, title string, format SpinnerUxType)
	// Stop the current spinner from the console and change the spinner bar for the lastMessage
	// Set lastMessage to empty string to clear the spinner message instead of a displaying a last message
	// If there is no spinner running, this is a no-op function
	StopSpinner(ctx context.Context, lastMessage string, format SpinnerUxType)
	// Preview mode brings an embedded console within the current session.
	// Use nil for options to use defaults.
	// Use the returned io.Writer to produce the output within the previewer
	ShowPreviewer(ctx context.Context, options *ShowPreviewerOptions) io.Writer
	// Finalize the preview mode from console.
	StopPreviewer(ctx context.Context, keepLogs bool)
	// Determines if there is a current spinner running.
	IsSpinnerRunning(ctx context.Context) bool
	// Determines if the current spinner is an interactive spinner, where messages are updated periodically.
	// If false, the spinner is non-interactive, which means messages are rendered as a new console message on each
	// call to ShowSpinner, even when the title is unchanged.
	IsSpinnerInteractive() bool
	// Prompts the user for a single value
	Prompt(ctx context.Context, options ConsoleOptions) (string, error)
	// Prompts the user to select a single value from a set of values
	Select(ctx context.Context, options ConsoleOptions) (int, error)
	// Prompts the user to select zero or more values from a set of values
	MultiSelect(ctx context.Context, options ConsoleOptions) ([]string, error)
	// Prompts the user to confirm an operation
	Confirm(ctx context.Context, options ConsoleOptions) (bool, error)
	// block terminal until the next enter
	WaitForEnter()
	// Writes a new line to the writer if there if the last two characters written are not '\n'
	EnsureBlankLine(ctx context.Context)
	// Sets the underlying writer for the console
	SetWriter(writer io.Writer)
	// Gets the underlying writer for the console
	GetWriter() io.Writer
	// Gets the standard input, output and error stream
	Handles() ConsoleHandles
	ConsoleShim
}

type AskerConsole struct {
	asker   Asker
	handles ConsoleHandles
	// the writer the console was constructed with, and what we reset to when SetWriter(nil) is called.
	defaultWriter io.Writer
	// the writer which output is written to.
	writer     io.Writer
	formatter  output.Formatter
	isTerminal bool
	noPrompt   bool

	showProgressMu sync.Mutex // ensures atomicity when swapping the current progress renderer (spinner or previewer)

	spinner             *yacspin.Spinner
	spinnerLineMu       sync.Mutex // secures spinnerCurrentTitle and the line of spinner text
	spinnerTerminalMode yacspin.TerminalMode
	spinnerCurrentTitle string

	previewer *progressLog

	currentIndent *atomic.String
	consoleWidth  *atomic.Int32
	// holds the last 2 bytes written by message or messageUX. This is used to detect when there is already an empty
	// line (\n\n)
	last2Byte [2]byte
}

type ConsoleOptions struct {
	Message      string
	Help         string
	Options      []string
	DefaultValue any

	// Prompt-only options

	IsPassword bool
	Suggest    func(input string) (completions []string)
}

type ConsoleHandles struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Sets the underlying writer for output the console or
// if writer is nil, sets it back to the default writer.
func (c *AskerConsole) SetWriter(writer io.Writer) {
	if writer == nil {
		writer = c.defaultWriter
	}

	c.writer = writer
}

func (c *AskerConsole) GetFormatter() output.Formatter {
	return c.formatter
}

func (c *AskerConsole) IsUnformatted() bool {
	return c.formatter == nil || c.formatter.Kind() == output.NoneFormat
}

// Prints out a message to the underlying console write
func (c *AskerConsole) Message(ctx context.Context, message string) {
	// Disable output when formatting is enabled
	if c.formatter != nil && c.formatter.Kind() == output.JsonFormat {
		// we call json.Marshal directly, because the formatter marshalls using indentation, and we would prefer
		// these objects be written on a single line.
		jsonMessage, err := json.Marshal(output.EventForMessage(message))
		if err != nil {
			panic(fmt.Sprintf("Message: unexpected error during marshaling for a valid object: %v", err))
		}
		fmt.Fprintln(c.writer, string(jsonMessage))
	} else if c.formatter == nil || c.formatter.Kind() == output.NoneFormat {
		c.println(ctx, message)
	} else {
		log.Println(message)
	}
	// Adding "\n" b/c calling Fprintln is adding one new line at the end to the msg
	c.updateLastBytes(message + "\n")
}

func (c *AskerConsole) updateLastBytes(msg string) {
	msgLen := len(msg)
	if msgLen == 0 {
		return
	}
	if msgLen < 2 {
		c.last2Byte[0] = c.last2Byte[1]
		c.last2Byte[1] = msg[msgLen-1]
		return
	}
	c.last2Byte[0] = msg[msgLen-2]
	c.last2Byte[1] = msg[msgLen-1]
}

func (c *AskerConsole) WarnForFeature(ctx context.Context, key alpha.FeatureId) {
	if shouldWarn(key) {
		c.MessageUxItem(ctx, &ux.MultilineMessage{
			Lines: []string{
				"",
				output.WithWarningFormat("WARNING: Feature '%s' is in alpha stage.", string(key)),
				fmt.Sprintf("To learn more about alpha features and their support, visit %s.",
					output.WithLinkFormat("https://aka.ms/azd-feature-stages")),
				"",
			},
		})
	}
}

// shouldWarn returns true if a warning should be emitted when using a given alpha feature.
func shouldWarn(key alpha.FeatureId) bool {
	noAlphaWarnings, err := strconv.ParseBool(os.Getenv("AZD_DEBUG_NO_ALPHA_WARNINGS"))

	return err != nil || !noAlphaWarnings
}

func (c *AskerConsole) MessageUxItem(ctx context.Context, item ux.UxItem) {
	if c.formatter != nil && c.formatter.Kind() == output.JsonFormat {
		// no need to check the spinner for json format, as the spinner won't start when using json format
		// instead, there would be a message about starting spinner
		json, _ := json.Marshal(item)
		fmt.Fprintln(c.writer, string(json))
		return
	}

	msg := item.ToString(c.currentIndent.Load())
	c.println(ctx, msg)
	// Adding "\n" b/c calling Fprintln is adding one new line at the end to the msg
	c.updateLastBytes(msg + "\n")
}

func (c *AskerConsole) println(ctx context.Context, msg string) {
	if c.spinner.Status() == yacspin.SpinnerRunning {
		c.StopSpinner(ctx, "", Step)
		// default non-format
		fmt.Fprintln(c.writer, msg)
		_ = c.spinner.Start()
	} else {
		fmt.Fprintln(c.writer, msg)
	}
}

func defaultShowPreviewerOptions() *ShowPreviewerOptions {
	return &ShowPreviewerOptions{
		MaxLineCount: 5,
	}
}

func (c *AskerConsole) ShowPreviewer(ctx context.Context, options *ShowPreviewerOptions) io.Writer {
	c.showProgressMu.Lock()
	defer c.showProgressMu.Unlock()

	// Pause any active spinner
	currentMsg := c.spinnerCurrentTitle
	_ = c.spinner.Pause()

	if options == nil {
		options = defaultShowPreviewerOptions()
	}

	c.previewer = NewProgressLog(options.MaxLineCount, options.Prefix, options.Title, c.currentIndent.Load()+currentMsg)
	c.previewer.Start()
	c.writer = c.previewer
	return &consolePreviewerWriter{
		previewer: &c.previewer,
	}
}

func (c *AskerConsole) StopPreviewer(ctx context.Context, keepLogs bool) {
	c.previewer.Stop(keepLogs)
	c.previewer = nil
	c.writer = c.defaultWriter

	_ = c.spinner.Unpause()
}

const cPostfix = "..."

// The line of text for the spinner, displayed in the format of: <prefix><spinner> <message>
type spinnerLine struct {
	// The prefix before the spinner.
	Prefix string

	// Charset that is used to animate the spinner.
	CharSet []string

	// The message to be displayed.
	Message string
}

func (c *AskerConsole) spinnerLine(title string, indent string) spinnerLine {
	spinnerLen := len(indent) + len(spinnerCharSet[0]) + 1 // adding one for the empty space before the message
	width := int(c.consoleWidth.Load())

	switch {
	case width <= 3: // show number of dots up to 3
		return spinnerLine{
			CharSet: spinnerShortCharSet[:width],
		}
	case width <= spinnerLen+len(cPostfix): // show number of dots
		return spinnerLine{
			CharSet: spinnerShortCharSet,
		}
	case width <= spinnerLen+len(title): // truncate title
		return spinnerLine{
			Prefix:  indent,
			CharSet: spinnerCharSet,
			Message: title[:width-spinnerLen-len(cPostfix)] + cPostfix,
		}
	default:
		return spinnerLine{
			Prefix:  indent,
			CharSet: spinnerCharSet,
			Message: title,
		}
	}
}

func (c *AskerConsole) ShowSpinner(ctx context.Context, title string, format SpinnerUxType) {
	c.showProgressMu.Lock()
	defer c.showProgressMu.Unlock()

	if c.formatter != nil && c.formatter.Kind() == output.JsonFormat {
		// Spinner is disabled when using json format.
		return
	}

	if c.previewer != nil {
		// spinner is not compatible with previewer.
		c.previewer.Header(c.currentIndent.Load() + title)
		return
	}

	c.spinnerLineMu.Lock()
	c.spinnerCurrentTitle = title

	indentPrefix := c.getIndent(format)
	line := c.spinnerLine(title, indentPrefix)
	c.spinner.Message(line.Message)
	_ = c.spinner.CharSet(line.CharSet)
	c.spinner.Prefix(line.Prefix)

	_ = c.spinner.Start()
	c.spinnerLineMu.Unlock()
}

// spinnerTerminalMode determines the appropriate terminal mode for the spinner based on the current environment,
// taking into account of environment variables that can control the terminal mode behavior.
func spinnerTerminalMode(isTerminal bool) yacspin.TerminalMode {
	nonInteractiveMode := yacspin.ForceNoTTYMode | yacspin.ForceDumbTerminalMode
	if !isTerminal {
		return nonInteractiveMode
	}

	// User override to force non-TTY behavior
	if os.Getenv("AZD_DEBUG_FORCE_NO_TTY") == "1" {
		return nonInteractiveMode
	}

	// By default, detect if we are running on CI and force no TTY mode if we are.
	// Allow for an override if this is not desired.
	shouldDetectCI := true
	if strVal, has := os.LookupEnv("AZD_TERM_SKIP_CI_DETECT"); has {
		skip, err := strconv.ParseBool(strVal)
		if err != nil {
			log.Println("AZD_TERM_SKIP_CI_DETECT is not a valid boolean value")
		} else if skip {
			shouldDetectCI = false
		}
	}

	if shouldDetectCI && resource.IsRunningOnCI() {
		return nonInteractiveMode
	}

	termMode := yacspin.ForceTTYMode
	if os.Getenv("TERM") == "dumb" {
		termMode |= yacspin.ForceDumbTerminalMode
	} else {
		termMode |= yacspin.ForceSmartTerminalMode
	}
	return termMode
}

var spinnerCharSet []string = []string{
	"|       |", "|=      |", "|==     |", "|===    |", "|====   |", "|=====  |", "|====== |",
	"|=======|", "| ======|", "|  =====|", "|   ====|", "|    ===|", "|     ==|", "|      =|",
}

var spinnerShortCharSet []string = []string{".", "..", "..."}

func setIndentation(spaces int) string {
	bytes := make([]byte, spaces)
	for i := range bytes {
		bytes[i] = byte(' ')
	}
	return string(bytes)
}

func (c *AskerConsole) getIndent(format SpinnerUxType) string {
	requiredSize := 2
	if requiredSize != len(c.currentIndent.Load()) {
		c.currentIndent.Store(setIndentation(requiredSize))
	}
	return c.currentIndent.Load()
}

func (c *AskerConsole) StopSpinner(ctx context.Context, lastMessage string, format SpinnerUxType) {
	if c.formatter != nil && c.formatter.Kind() == output.JsonFormat {
		// Spinner is disabled when using json format.
		return
	}

	// Do nothing when it is already stopped
	if c.spinner.Status() == yacspin.SpinnerStopped {
		return
	}

	c.spinnerLineMu.Lock()
	c.spinnerCurrentTitle = ""
	// Update style according to MessageUxType
	if lastMessage != "" {
		lastMessage = c.getStopChar(format) + " " + lastMessage
	}

	c.spinner.StopMessage(lastMessage)
	_ = c.spinner.Stop()
	c.spinnerLineMu.Unlock()
}

func (c *AskerConsole) IsSpinnerRunning(ctx context.Context) bool {
	return c.spinner.Status() != yacspin.SpinnerStopped
}

func (c *AskerConsole) IsSpinnerInteractive() bool {
	return c.spinnerTerminalMode&yacspin.ForceTTYMode > 0
}

var donePrefix string = output.WithSuccessFormat("(✓) Done:")

func (c *AskerConsole) getStopChar(format SpinnerUxType) string {
	var stopChar string
	switch format {
	case StepDone:
		stopChar = donePrefix
	case StepFailed:
		stopChar = output.WithErrorFormat("(x) Failed:")
	case StepWarning:
		stopChar = output.WithWarningFormat("(!) Warning:")
	case StepSkipped:
		stopChar = output.WithGrayFormat("(-) Skipped:")
	}
	return fmt.Sprintf("%s%s", c.getIndent(format), stopChar)
}

func promptFromOptions(options ConsoleOptions) survey.Prompt {
	if options.IsPassword {
		return &survey.Password{
			Message: options.Message,
		}
	}

	var defaultValue string
	if value, ok := options.DefaultValue.(string); ok {
		defaultValue = value
	}
	return &survey.Input{
		Message: options.Message,
		Default: defaultValue,
		Help:    options.Help,
		Suggest: options.Suggest,
	}
}

// cAfterIO is a sentinel used after Input/Output operations as the state for the last 2-bytes written.
// For example, after running Prompt or Confirm, the last characters on the terminal should be any char (represented by the
// 0 in the sentinel), followed by a new line.
const cAfterIO = "0\n"

// Prompts the user for a single value
func (c *AskerConsole) Prompt(ctx context.Context, options ConsoleOptions) (string, error) {
	var response string

	err := c.doInteraction(func(c *AskerConsole) error {
		return c.asker(promptFromOptions(options), &response)
	})
	if err != nil {
		return response, err
	}
	c.updateLastBytes(cAfterIO)
	return response, nil
}

// Prompts the user to select from a set of values
func (c *AskerConsole) Select(ctx context.Context, options ConsoleOptions) (int, error) {
	survey := &survey.Select{
		Message: options.Message,
		Options: options.Options,
		Default: options.DefaultValue,
		Help:    options.Help,
	}

	var response int

	err := c.doInteraction(func(c *AskerConsole) error {
		return c.asker(survey, &response)
	})
	if err != nil {
		return -1, err
	}

	c.updateLastBytes(cAfterIO)
	return response, nil
}

func (c *AskerConsole) MultiSelect(ctx context.Context, options ConsoleOptions) ([]string, error) {
	survey := &survey.MultiSelect{
		Message: options.Message,
		Options: options.Options,
		Default: options.DefaultValue,
		Help:    options.Help,
	}

	var response []string

	err := c.doInteraction(func(c *AskerConsole) error {
		return c.asker(survey, &response)
	})
	if err != nil {
		return nil, err
	}

	return response, nil
}

// Prompts the user to confirm an operation
func (c *AskerConsole) Confirm(ctx context.Context, options ConsoleOptions) (bool, error) {
	var defaultValue bool
	if value, ok := options.DefaultValue.(bool); ok {
		defaultValue = value
	}

	survey := &survey.Confirm{
		Message: options.Message,
		Help:    options.Help,
		Default: defaultValue,
	}

	var response bool

	err := c.doInteraction(func(c *AskerConsole) error {
		return c.asker(survey, &response)
	})
	if err != nil {
		return false, err
	}

	c.updateLastBytes(cAfterIO)
	return response, nil
}

const c_newLine = '\n'

func (c *AskerConsole) EnsureBlankLine(ctx context.Context) {
	if c.last2Byte[0] == c_newLine && c.last2Byte[1] == c_newLine {
		return
	}
	if c.last2Byte[1] != c_newLine {
		c.Message(ctx, "\n")
		return
	}
	// [1] is '\n' but [0] is not. One new line missing
	c.Message(ctx, "")
}

// wait until the next enter
func (c *AskerConsole) WaitForEnter() {
	if c.noPrompt {
		return
	}

	inputScanner := bufio.NewScanner(c.handles.Stdin)
	if scan := inputScanner.Scan(); !scan {
		if err := inputScanner.Err(); err != nil {
			log.Printf("error while waiting for enter: %v", err)
		}
	}
}

// Gets the underlying writer for the console
func (c *AskerConsole) GetWriter() io.Writer {
	return c.writer
}

func (c *AskerConsole) Handles() ConsoleHandles {
	return c.handles
}

func getConsoleWidth() int {
	width, _ := consolesize.GetConsoleSize()
	return width
}

func (c *AskerConsole) handleResize(width int) {
	c.consoleWidth.Store(int32(width))

	c.spinnerLineMu.Lock()
	if c.spinner.Status() == yacspin.SpinnerRunning {
		line := c.spinnerLine(c.spinnerCurrentTitle, c.currentIndent.Load())
		c.spinner.Message(line.Message)
		_ = c.spinner.CharSet(line.CharSet)
		c.spinner.Prefix(line.Prefix)
	}
	c.spinnerLineMu.Unlock()
}

func watchConsoleWidth(c *AskerConsole) {
	if runtime.GOOS == "windows" {
		go func() {
			prevWidth := getConsoleWidth()
			for {
				time.Sleep(time.Millisecond * 250)
				width := getConsoleWidth()

				if prevWidth != width {
					c.handleResize(width)
				}
				prevWidth = width
			}
		}()
	} else {
		// avoid taking a dependency on syscall.SIGWINCH (unix-only constant) directly
		const SIGWINCH = syscall.Signal(0x1c)
		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, SIGWINCH)
		go func() {
			for range signalChan {
				c.handleResize(getConsoleWidth())
			}
		}()
	}
}

// Creates a new console with the specified writer, handles and formatter.
func NewConsole(noPrompt bool, isTerminal bool, w io.Writer, handles ConsoleHandles, formatter output.Formatter) Console {
	asker := NewAsker(noPrompt, isTerminal, handles.Stdout, handles.Stdin)

	c := &AskerConsole{
		asker:         asker,
		handles:       handles,
		defaultWriter: w,
		writer:        w,
		formatter:     formatter,
		isTerminal:    isTerminal,
		consoleWidth:  atomic.NewInt32(int32(getConsoleWidth())),
		currentIndent: atomic.NewString(""),
		noPrompt:      noPrompt,
	}

	spinnerConfig := yacspin.Config{
		Frequency:    200 * time.Millisecond,
		Writer:       c.writer,
		Suffix:       " ",
		TerminalMode: spinnerTerminalMode(isTerminal),
		CharSet:      spinnerCharSet,
	}
	c.spinner, _ = yacspin.New(spinnerConfig)
	c.spinnerTerminalMode = spinnerConfig.TerminalMode

	go watchConsoleWidth(c)
	return c
}

func GetStepResultFormat(result error) SpinnerUxType {
	formatResult := StepDone
	if result != nil {
		formatResult = StepFailed
	}
	return formatResult
}

// Handle doing interactive calls. It checks if there's a spinner running to pause it before doing interactive actions.
func (c *AskerConsole) doInteraction(promptFn func(c *AskerConsole) error) error {
	if c.spinner.Status() == yacspin.SpinnerRunning {
		_ = c.spinner.Pause()

		// Ensure the spinner is always resumed
		defer func() {
			_ = c.spinner.Unpause()
		}()
	}

	// Track total time for promptFn.
	// It includes the time spent in rendering the prompt (likely <1ms)
	// before the user has a chance to interact with the prompt.
	start := time.Now()
	defer func() {
		tracing.InteractTimeMs.Add(time.Since(start).Milliseconds())
	}()

	// Execute the interactive prompt
	return promptFn(c)
}
