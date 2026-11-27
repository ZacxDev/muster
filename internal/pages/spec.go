package pages

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The block types (docs/pages.md#blocks).
const (
	BlockMarkdown = "markdown"
	BlockQuestion = "question"
	BlockChoice   = "choice"
	BlockForm     = "form"
	BlockUpload   = "upload"
	BlockApproval = "approval"
	BlockHTML     = "html"
	BlockGallery  = "gallery"
)

// A gallery's layouts (Block.Layout): a grid of thumbnails (the default), or
// one image per row.
const (
	GalleryGrid = "grid"
	GalleryRows = "rows"
)

// CustomComponent is the component a custom script's muster.submit and
// muster.upload name by default. It is accepted only by a version with an html
// block, and no block may be called it.
const CustomComponent = "custom"

// The form field types.
const (
	FieldText     = "text"
	FieldTextarea = "textarea"
	FieldEmail    = "email"
	FieldNumber   = "number"
	FieldDate     = "date"
	FieldSelect   = "select"
	FieldCheckbox = "checkbox"
)

// The per-block bounds.
const (
	maxLabelRunes    = 500
	maxOptions       = 20
	maxOptionRunes   = 200
	maxFields        = 30
	maxAnswerRunes   = 4000
	maxTextRunes     = 1000
	maxTextareaRunes = 8000
	maxSubmitLabel   = 60
	// MaxGalleryItems bounds a gallery's items; maxCaptionRunes and
	// maxGroupRunes an item's caption and group heading.
	MaxGalleryItems = 200
	maxCaptionRunes = 120
	maxGroupRunes   = 80
)

// Block is one block of a version. Which fields a block reads depends on its
// Type; Parse refuses a field its type does not read, so an agent is told
// rather than silently ignored.
type Block struct {
	Type string `json:"type"`
	// ID names a component (every type but markdown and html): what a submit
	// and a response carry. Unique within a version.
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	// Text is a markdown block's source.
	Text string `json:"text,omitempty"`
	// HTML is an html block's markup and script, served verbatim inside the
	// sandbox.
	HTML string `json:"html,omitempty"`
	// Required: a question's answer, a choice's pick.
	Required bool `json:"required,omitempty"`
	// Multiline: a question's box is a textarea.
	Multiline bool `json:"multiline,omitempty"`
	// Options are a choice's answers.
	Options []string `json:"options,omitempty"`
	// Multiple: a choice takes several answers; an upload several files.
	Multiple bool `json:"multiple,omitempty"`
	// Fields are a form's fields.
	Fields []Field `json:"fields,omitempty"`
	// SubmitLabel is a component's button text ("Send" when empty).
	SubmitLabel string `json:"submit_label,omitempty"`
	// Comment: an approval also takes a comment.
	Comment bool `json:"comment,omitempty"`
	// Items are a gallery's images, in order.
	Items []GalleryItem `json:"items,omitempty"`
	// Pick, when set, lets a gallery's viewer pick items and save the picks.
	Pick *GalleryPick `json:"pick,omitempty"`
	// Layout is a gallery's layout: GalleryGrid ("" too) or GalleryRows.
	Layout string `json:"layout,omitempty"`
}

// GalleryItem is one image of a gallery: an asset of the page's (asset:<id>),
// its caption (its alt text and its name in a pick) and the group it is
// headed under.
type GalleryItem struct {
	Asset   string `json:"asset"`
	Caption string `json:"caption,omitempty"`
	Group   string `json:"group,omitempty"`
}

// GalleryPick is how a gallery's items are picked: several (Multiple nil or
// true) or one, at least one when Required, saved by a button reading
// SubmitLabel ("Save" when empty).
type GalleryPick struct {
	Multiple    *bool  `json:"multiple,omitempty"`
	Required    bool   `json:"required,omitempty"`
	SubmitLabel string `json:"submit_label,omitempty"`
}

// PicksSeveral reports whether p picks several items (the default) rather
// than one.
func (p *GalleryPick) PicksSeveral() bool { return p != nil && (p.Multiple == nil || *p.Multiple) }

// Field is one form field.
type Field struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type"`
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"`
}

var (
	componentIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	fieldNamePattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	datePattern        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// galleryAssetPattern is a gallery item's asset: exactly a reference.
	galleryAssetPattern = regexp.MustCompile(`^asset:[A-Za-z0-9_-]{22}$`)
)

// ValidComponent reports whether s is shaped like a component id (0116's CHECK
// on page_responses.component is the same shape, CustomComponent included).
func ValidComponent(s string) bool { return componentIDPattern.MatchString(s) }

// invalid is an ErrInvalid with a sentence an agent can act on.
func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// CleanTitle trims a title and checks it: 1 to MaxTitleRunes characters, no
// control or text-direction characters.
func CleanTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", invalid("the title is empty")
	case utf8.RuneCountInString(s) > MaxTitleRunes:
		return "", invalid("the title is longer than %d characters", MaxTitleRunes)
	case !plainLine(s):
		return "", invalid("the title holds a control or text-direction character")
	}
	return s, nil
}

// plainLine reports whether s holds no control, format (bidi) or line-break
// character.
func plainLine(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// Parse reads a version's blocks from raw (a JSON array), refusing an unknown
// field, and validates them (Validate). It returns the blocks and their size
// as stored.
func Parse(raw []byte) ([]Block, int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, 0, invalid("blocks is required: a list of blocks")
	}
	if len(raw) > MaxBlocksInputBytes {
		return nil, 0, invalid("the blocks are larger than %d KiB", MaxVersionBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var blocks []Block
	if err := dec.Decode(&blocks); err != nil {
		return nil, 0, invalid("blocks is not a list of blocks: %v", err)
	}
	if dec.More() {
		return nil, 0, invalid("blocks is followed by more JSON")
	}
	return Validate(blocks)
}

// Validate checks blocks — each block's fields, component ids unique and not
// "custom", the counts and the size — and returns them with their size as
// stored (their JSON).
func Validate(blocks []Block) ([]Block, int, error) {
	if len(blocks) == 0 {
		return nil, 0, invalid("a page needs at least one block")
	}
	if len(blocks) > MaxBlocks {
		return nil, 0, invalid("a page has at most %d blocks", MaxBlocks)
	}
	ids := map[string]bool{}
	for i := range blocks {
		b := &blocks[i]
		if err := validateBlock(b); err != nil {
			return nil, 0, fmt.Errorf("block %d (%s): %w", i+1, b.Type, err)
		}
		if b.ID != "" {
			if ids[strings.ToLower(b.ID)] {
				return nil, 0, invalid("block %d: id %q is used twice", i+1, b.ID)
			}
			ids[strings.ToLower(b.ID)] = true
		}
	}
	enc, err := json.Marshal(blocks)
	if err != nil {
		return nil, 0, err
	}
	if len(enc) > MaxVersionBytes {
		return nil, 0, invalid("the blocks are %d KiB; a version holds at most %d KiB (images inline as data: URIs count; attach a large image or a video with page_asset_upload and use its asset: ref)", len(enc)>>10, MaxVersionBytes>>10)
	}
	return blocks, len(enc), nil
}

// component reports whether a block of type t is a component (has an id).
func component(t string) bool {
	switch t {
	case BlockQuestion, BlockChoice, BlockForm, BlockUpload, BlockApproval, BlockGallery:
		return true
	}
	return false
}

// answerableTypes are the built-in components a person ANSWERS: each records a
// response under its own id when sent. A gallery is one only with a pick
// (Block.Pick), so it is not listed here but asked of separately
// (Store.Unanswered). A markdown block answers nothing, and an html block's
// script may send anything or nothing (CustomComponent), so neither can be
// said to be waiting.
var answerableTypes = []string{BlockQuestion, BlockChoice, BlockForm, BlockUpload, BlockApproval}

func validateBlock(b *Block) error {
	b.Label = strings.TrimSpace(b.Label)
	b.SubmitLabel = strings.TrimSpace(b.SubmitLabel)
	if utf8.RuneCountInString(b.Label) > maxLabelRunes {
		return invalid("the label is longer than %d characters", maxLabelRunes)
	}
	if utf8.RuneCountInString(b.SubmitLabel) > maxSubmitLabel {
		return invalid("submit_label is longer than %d characters", maxSubmitLabel)
	}
	switch b.Type {
	case BlockMarkdown, BlockQuestion, BlockChoice, BlockForm, BlockUpload, BlockApproval, BlockHTML, BlockGallery:
	default:
		return invalid("unknown block type %q (want markdown, question, choice, form, upload, approval, html or gallery)", b.Type)
	}
	if component(b.Type) {
		switch {
		case b.ID == "":
			return invalid("a %s needs an id", b.Type)
		case !componentIDPattern.MatchString(b.ID):
			return invalid("id %q must be a letter then up to 63 letters, digits, _ or -", b.ID)
		case strings.EqualFold(b.ID, CustomComponent):
			return invalid("id %q is reserved for custom script", CustomComponent)
		}
	} else if b.ID != "" {
		return invalid("a %s block takes no id", b.Type)
	}
	// What each type does NOT read is refused, so a misplaced field is an
	// error rather than a silent no-op.
	unread := func(name string, set bool) error {
		if set {
			return invalid("a %s block does not take %s", b.Type, name)
		}
		return nil
	}
	checks := map[string]bool{
		"text":         b.Text != "",
		"html":         b.HTML != "",
		"required":     b.Required,
		"multiline":    b.Multiline,
		"options":      len(b.Options) > 0,
		"multiple":     b.Multiple,
		"fields":       len(b.Fields) > 0,
		"submit_label": b.SubmitLabel != "",
		"comment":      b.Comment,
		"label":        b.Label != "",
		"items":        len(b.Items) > 0,
		"pick":         b.Pick != nil,
		"layout":       b.Layout != "",
	}
	reads := map[string][]string{
		BlockMarkdown: {"text"},
		BlockHTML:     {"html"},
		BlockQuestion: {"label", "required", "multiline", "submit_label"},
		BlockChoice:   {"label", "required", "options", "multiple", "submit_label"},
		BlockForm:     {"label", "fields", "submit_label"},
		BlockUpload:   {"label", "multiple", "submit_label"},
		BlockApproval: {"label", "comment"},
		BlockGallery:  {"label", "items", "pick", "layout"},
	}[b.Type]
	for _, name := range []string{"text", "html", "required", "multiline", "options", "multiple", "fields", "submit_label", "comment", "label", "items", "pick", "layout"} {
		if !contains(reads, name) {
			if err := unread(name, checks[name]); err != nil {
				return err
			}
		}
	}
	switch b.Type {
	case BlockMarkdown:
		if strings.TrimSpace(b.Text) == "" {
			return invalid("a markdown block needs text")
		}
		if len(b.Text) > MaxMarkdownBytes {
			return invalid("a markdown block holds at most %d KiB", MaxMarkdownBytes>>10)
		}
	case BlockHTML:
		if strings.TrimSpace(b.HTML) == "" {
			return invalid("an html block needs html")
		}
		if len(b.HTML) > MaxHTMLBytes {
			return invalid("an html block holds at most %d KiB", MaxHTMLBytes>>10)
		}
	case BlockQuestion, BlockApproval, BlockUpload:
		if b.Label == "" {
			return invalid("a %s needs a label", b.Type)
		}
	case BlockChoice:
		if b.Label == "" {
			return invalid("a choice needs a label")
		}
		opts, err := cleanOptions(b.Options)
		if err != nil {
			return err
		}
		b.Options = opts
	case BlockGallery:
		return validateGallery(b)
	case BlockForm:
		if len(b.Fields) == 0 {
			return invalid("a form needs fields")
		}
		if len(b.Fields) > maxFields {
			return invalid("a form has at most %d fields", maxFields)
		}
		names := map[string]bool{}
		for i := range b.Fields {
			f := &b.Fields[i]
			if err := validateField(f); err != nil {
				return fmt.Errorf("field %d: %w", i+1, err)
			}
			if names[f.Name] {
				return invalid("field name %q is used twice", f.Name)
			}
			names[f.Name] = true
		}
	}
	return nil
}

// validateGallery checks a gallery's items (1 to MaxGalleryItems, each an
// asset reference — that it is one of the page's own is the store's check,
// AssetRefs — with a one-line caption and group), its layout and its pick.
//
// A caption or group may not hold anything shaped like an asset reference
// (assetRefPattern): the SDK swaps every asset:<id> of the page in the
// rendered markup for its placeholder (asset.go), which would rewrite the
// caption the gallery sends with a pick, and checkGalleryPicks would then
// refuse every Save ("caption is not the one sent").
func validateGallery(b *Block) error {
	switch {
	case len(b.Items) == 0:
		return invalid("a gallery needs items")
	case len(b.Items) > MaxGalleryItems:
		return invalid("a gallery has at most %d items", MaxGalleryItems)
	}
	for i := range b.Items {
		it := &b.Items[i]
		it.Asset = strings.TrimSpace(it.Asset)
		it.Caption = strings.TrimSpace(it.Caption)
		it.Group = strings.TrimSpace(it.Group)
		switch {
		case !galleryAssetPattern.MatchString(it.Asset):
			return invalid("item %d: asset %q is not an asset reference (asset:<id>, as page_asset_upload returned it)", i+1, cutRunes(it.Asset, 40))
		case utf8.RuneCountInString(it.Caption) > maxCaptionRunes:
			return invalid("item %d: the caption is longer than %d characters", i+1, maxCaptionRunes)
		case !plainLine(it.Caption):
			return invalid("item %d: the caption holds a control or text-direction character", i+1)
		case assetRefPattern.MatchString(it.Caption):
			return invalid("item %d: the caption holds an asset reference (%s); a caption is plain words — put the image in an item of its own", i+1, cutRunes(assetRefPattern.FindString(it.Caption), 40))
		case utf8.RuneCountInString(it.Group) > maxGroupRunes:
			return invalid("item %d: the group is longer than %d characters", i+1, maxGroupRunes)
		case !plainLine(it.Group):
			return invalid("item %d: the group holds a control or text-direction character", i+1)
		case assetRefPattern.MatchString(it.Group):
			return invalid("item %d: the group holds an asset reference (%s); a group is plain words", i+1, cutRunes(assetRefPattern.FindString(it.Group), 40))
		}
	}
	switch b.Layout {
	case "", GalleryGrid, GalleryRows:
	default:
		return invalid("layout %q is not grid or rows", b.Layout)
	}
	if b.Pick != nil {
		b.Pick.SubmitLabel = strings.TrimSpace(b.Pick.SubmitLabel)
		if utf8.RuneCountInString(b.Pick.SubmitLabel) > maxSubmitLabel {
			return invalid("pick.submit_label is longer than %d characters", maxSubmitLabel)
		}
	}
	return nil
}

func validateField(f *Field) error {
	f.Label = strings.TrimSpace(f.Label)
	if !fieldNamePattern.MatchString(f.Name) {
		return invalid("name %q must be a letter then up to 63 letters, digits or _", f.Name)
	}
	if utf8.RuneCountInString(f.Label) > maxLabelRunes {
		return invalid("the label is longer than %d characters", maxLabelRunes)
	}
	switch f.Type {
	case FieldText, FieldTextarea, FieldEmail, FieldNumber, FieldDate, FieldCheckbox:
		if len(f.Options) > 0 {
			return invalid("a %s field takes no options", f.Type)
		}
	case FieldSelect:
		opts, err := cleanOptions(f.Options)
		if err != nil {
			return err
		}
		f.Options = opts
	default:
		return invalid("unknown field type %q (want text, textarea, email, number, date, select or checkbox)", f.Type)
	}
	return nil
}

func cleanOptions(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, invalid("it needs options")
	}
	if len(in) > maxOptions {
		return nil, invalid("at most %d options", maxOptions)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, o := range in {
		o = strings.TrimSpace(o)
		switch {
		case o == "":
			return nil, invalid("an option is empty")
		case utf8.RuneCountInString(o) > maxOptionRunes:
			return nil, invalid("an option is longer than %d characters", maxOptionRunes)
		case seen[o]:
			return nil, invalid("option %q is listed twice", o)
		}
		seen[o] = true
		out = append(out, o)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// HasHTML reports whether blocks hold an html block (so custom script may
// submit as CustomComponent).
func HasHTML(blocks []Block) bool {
	for _, b := range blocks {
		if b.Type == BlockHTML {
			return true
		}
	}
	return false
}

// Find is the component block named id, if blocks have one.
func Find(blocks []Block, id string) (Block, bool) {
	for _, b := range blocks {
		if b.ID != "" && b.ID == id {
			return b, true
		}
	}
	return Block{}, false
}

// UploadTarget reports whether a file may be uploaded as component of
// blocks: an upload block's id, or CustomComponent on a version with an html
// block.
func UploadTarget(blocks []Block, component string) bool {
	if component == CustomComponent {
		return HasHTML(blocks)
	}
	b, ok := Find(blocks, component)
	return ok && b.Type == BlockUpload
}

// CheckSubmit checks a viewer's submit to component of blocks and returns its
// data as stored (normalised JSON). The component must be one of the blocks'
// (or CustomComponent on a version with an html block, whose data is any JSON
// up to MaxSubmitBytes), and the data must be what that component sends.
func CheckSubmit(blocks []Block, component string, data json.RawMessage) (json.RawMessage, error) {
	if len(data) > MaxSubmitBytes {
		return nil, invalid("the data is larger than %d KiB", MaxSubmitBytes>>10)
	}
	if component == CustomComponent {
		if !HasHTML(blocks) {
			return nil, invalid("this page has no custom script")
		}
		if len(bytes.TrimSpace(data)) == 0 {
			data = json.RawMessage("null")
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, invalid("the data is not JSON")
		}
		return json.Marshal(map[string]any{"value": v})
	}
	b, ok := Find(blocks, component)
	if !ok {
		return nil, invalid("this page has no component %q", component)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	switch b.Type {
	case BlockQuestion:
		var in struct {
			Answer string `json:"answer"`
		}
		if err := dec.Decode(&in); err != nil {
			return nil, invalid("a question sends {answer}")
		}
		in.Answer = strings.TrimSpace(in.Answer)
		if b.Required && in.Answer == "" {
			return nil, invalid("an answer is required")
		}
		if utf8.RuneCountInString(in.Answer) > maxAnswerRunes {
			return nil, invalid("the answer is longer than %d characters", maxAnswerRunes)
		}
		return json.Marshal(in)
	case BlockChoice:
		var in struct {
			Choices []string `json:"choices"`
		}
		if err := dec.Decode(&in); err != nil {
			return nil, invalid("a choice sends {choices}")
		}
		seen := map[string]bool{}
		for _, c := range in.Choices {
			if !contains(b.Options, c) {
				return nil, invalid("%q is not one of the choices", c)
			}
			if seen[c] {
				return nil, invalid("%q is chosen twice", c)
			}
			seen[c] = true
		}
		if !b.Multiple && len(in.Choices) > 1 {
			return nil, invalid("choose one")
		}
		if b.Required && len(in.Choices) == 0 {
			return nil, invalid("a choice is required")
		}
		if in.Choices == nil {
			in.Choices = []string{}
		}
		return json.Marshal(in)
	case BlockForm:
		var in struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if err := dec.Decode(&in); err != nil {
			return nil, invalid("a form sends {fields}")
		}
		out := map[string]any{}
		for name := range in.Fields {
			if !fieldNamed(b.Fields, name) {
				return nil, invalid("this form has no field %q", name)
			}
		}
		for _, f := range b.Fields {
			v, err := checkField(f, in.Fields[f.Name])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fieldName(f), err)
			}
			out[f.Name] = v
		}
		return json.Marshal(map[string]any{"fields": out})
	case BlockApproval:
		var in struct {
			Decision string `json:"decision"`
			Comment  string `json:"comment"`
		}
		if err := dec.Decode(&in); err != nil {
			return nil, invalid("an approval sends {decision, comment?}")
		}
		if in.Decision != "approve" && in.Decision != "decline" {
			return nil, invalid("the decision is approve or decline")
		}
		in.Comment = strings.TrimSpace(in.Comment)
		if in.Comment != "" && !b.Comment {
			return nil, invalid("this approval takes no comment")
		}
		if utf8.RuneCountInString(in.Comment) > maxAnswerRunes {
			return nil, invalid("the comment is longer than %d characters", maxAnswerRunes)
		}
		return json.Marshal(in)
	case BlockUpload:
		return nil, invalid("%q is an upload: send a file", component)
	case BlockGallery:
		return checkGalleryPicks(b, dec)
	}
	return nil, invalid("%q takes no submit", component)
}

// GalleryPicked is one picked item of a gallery as a submit sends it and as
// it is stored: the item's index in the block (from 0), its asset and its
// caption (none when the item has none).
type GalleryPicked struct {
	Index   int    `json:"index"`
	Asset   string `json:"asset"`
	Caption string `json:"caption,omitempty"`
}

// checkGalleryPicks checks a gallery's {picks} — each an item of the block,
// named by its index AND its asset (and its caption, if sent) so a pick saved
// against one version cannot silently land on another item — and returns
// them as stored: in the block's order, each with the item's own asset and
// caption.
func checkGalleryPicks(b Block, dec *json.Decoder) (json.RawMessage, error) {
	if b.Pick == nil {
		return nil, invalid("this gallery takes no picks")
	}
	var in struct {
		Picks []struct {
			Index   *int    `json:"index"`
			Asset   string  `json:"asset"`
			Caption *string `json:"caption"`
		} `json:"picks"`
	}
	if err := dec.Decode(&in); err != nil {
		return nil, invalid("a gallery sends {picks: [{index, asset, caption?}]}")
	}
	seen := map[int]bool{}
	for _, p := range in.Picks {
		if p.Index == nil || *p.Index < 0 || *p.Index >= len(b.Items) {
			return nil, invalid("a pick's index is not one of the gallery's %d items", len(b.Items))
		}
		it := b.Items[*p.Index]
		if p.Asset != it.Asset {
			return nil, invalid("item %d's asset is not %q", *p.Index, cutRunes(p.Asset, 40))
		}
		if p.Caption != nil && *p.Caption != it.Caption {
			return nil, invalid("item %d's caption is not the one sent", *p.Index)
		}
		if seen[*p.Index] {
			return nil, invalid("item %d is picked twice", *p.Index)
		}
		seen[*p.Index] = true
	}
	if !b.Pick.PicksSeveral() && len(seen) > 1 {
		return nil, invalid("pick one")
	}
	if b.Pick.Required && len(seen) == 0 {
		return nil, invalid("a pick is required")
	}
	out := struct {
		Picks []GalleryPicked `json:"picks"`
	}{Picks: []GalleryPicked{}}
	for i, it := range b.Items {
		if seen[i] {
			out.Picks = append(out.Picks, GalleryPicked{Index: i, Asset: it.Asset, Caption: it.Caption})
		}
	}
	return json.Marshal(out)
}

func fieldNamed(fields []Field, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

func fieldName(f Field) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

// checkField checks one field's value and returns it as stored: a string, a
// number or a bool (checkbox), or "" / nil when absent and not required.
func checkField(f Field, raw json.RawMessage) (any, error) {
	absent := len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
	if f.Type == FieldCheckbox {
		if absent {
			if f.Required {
				return nil, invalid("it must be ticked")
			}
			return false, nil
		}
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, invalid("it is true or false")
		}
		if f.Required && !v {
			return nil, invalid("it must be ticked")
		}
		return v, nil
	}
	var s string
	if !absent {
		if f.Type == FieldNumber {
			var n json.Number
			if err := json.Unmarshal(raw, &n); err == nil {
				s = n.String()
			} else if err := json.Unmarshal(raw, &s); err != nil {
				return nil, invalid("it is a number")
			}
		} else if err := json.Unmarshal(raw, &s); err != nil {
			return nil, invalid("it is text")
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		if f.Required {
			return nil, invalid("it is required")
		}
		if f.Type == FieldNumber {
			return nil, nil
		}
		return "", nil
	}
	limit := maxTextRunes
	if f.Type == FieldTextarea {
		limit = maxTextareaRunes
	}
	if utf8.RuneCountInString(s) > limit {
		return nil, invalid("it is longer than %d characters", limit)
	}
	switch f.Type {
	case FieldEmail:
		at := strings.IndexByte(s, '@')
		if at <= 0 || at == len(s)-1 || strings.ContainsAny(s, " \t\r\n") {
			return nil, invalid("it is not an email address")
		}
	case FieldNumber:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, invalid("it is not a number")
		}
		return n, nil
	case FieldDate:
		if !datePattern.MatchString(s) {
			return nil, invalid("it is a date, YYYY-MM-DD")
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return nil, invalid("it is not a real date")
		}
	case FieldSelect:
		if !contains(f.Options, s) {
			return nil, invalid("%q is not one of its options", s)
		}
	case FieldText:
		if strings.ContainsAny(s, "\r\n") {
			return nil, invalid("it is one line")
		}
	}
	return s, nil
}
