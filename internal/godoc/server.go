// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.16
// +build go1.16

package godoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/token"
	htmlpkg "html"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/website/internal/spec"
	"golang.org/x/website/internal/texthtml"
)

type DocTree struct {
	fs   fs.FS
	root *Directory
}

func NewDocTree(fsys fs.FS) *DocTree {
	src := newDirTree(fsys, token.NewFileSet(), "/src")
	root := &Directory{
		Path: "/",
		Dirs: []*Directory{src},
	}
	return &DocTree{
		fs:   fsys,
		root: root,
	}
}

// docServer serves a package doc tree (/cmd or /pkg).
type docServer struct {
	p *Presentation
	d *DocTree
}

// GetPageInfo returns the PageInfo for a package directory abspath. If the
// parameter genAST is set, an AST containing only the package exports is
// computed (PageInfo.PAst), otherwise package documentation (PageInfo.Doc)
// is extracted from the AST. If there is no corresponding package in the
// directory, PageInfo.PAst and PageInfo.PDoc are nil. If there are no sub-
// directories, PageInfo.Dirs is nil. If an error occurred, PageInfo.Err is
// set to the respective error but the error is not logged.
func (d *DocTree) GetPageInfo(abspath, relpath string, mode PageInfoMode, goos, goarch string) *PageInfo {
	info := &PageInfo{Dirname: abspath, Mode: mode}

	// Restrict to the package files that would be used when building
	// the package on this system.  This makes sure that if there are
	// separate implementations for, say, Windows vs Unix, we don't
	// jumble them all together.
	// Note: If goos/goarch aren't set, the current binary's GOOS/GOARCH
	// are used.
	ctxt := build.Default
	ctxt.IsAbsPath = pathpkg.IsAbs
	ctxt.IsDir = func(path string) bool {
		fi, err := fs.Stat(d.fs, toFS(filepath.ToSlash(path)))
		return err == nil && fi.IsDir()
	}
	ctxt.ReadDir = func(dir string) ([]os.FileInfo, error) {
		f, err := fs.ReadDir(d.fs, toFS(filepath.ToSlash(dir)))
		filtered := make([]os.FileInfo, 0, len(f))
		for _, i := range f {
			if mode&NoFiltering != 0 || i.Name() != "internal" {
				info, err := i.Info()
				if err == nil {
					filtered = append(filtered, info)
				}
			}
		}
		return filtered, err
	}
	ctxt.OpenFile = func(name string) (r io.ReadCloser, err error) {
		data, err := fs.ReadFile(d.fs, toFS(filepath.ToSlash(name)))
		if err != nil {
			return nil, err
		}
		return ioutil.NopCloser(bytes.NewReader(data)), nil
	}

	// Make the syscall/js package always visible by default.
	// It defaults to the host's GOOS/GOARCH, and golang.org's
	// linux/amd64 means the wasm syscall/js package was blank.
	// And you can't run godoc on js/wasm anyway, so host defaults
	// don't make sense here.
	if goos == "" && goarch == "" && relpath == "syscall/js" {
		goos, goarch = "js", "wasm"
	}
	if goos != "" {
		ctxt.GOOS = goos
	}
	if goarch != "" {
		ctxt.GOARCH = goarch
	}

	pkginfo, err := ctxt.ImportDir(abspath, 0)
	// continue if there are no Go source files; we still want the directory info
	if _, nogo := err.(*build.NoGoError); err != nil && !nogo {
		info.Err = err
		return info
	}

	// collect package files
	pkgname := pkginfo.Name
	pkgfiles := append(pkginfo.GoFiles, pkginfo.CgoFiles...)
	if len(pkgfiles) == 0 {
		// Commands written in C have no .go files in the build.
		// Instead, documentation may be found in an ignored file.
		// The file may be ignored via an explicit +build ignore
		// constraint (recommended), or by defining the package
		// documentation (historic).
		pkgname = "main" // assume package main since pkginfo.Name == ""
		pkgfiles = pkginfo.IgnoredGoFiles
	}

	// get package information, if any
	if len(pkgfiles) > 0 {
		// build package AST
		fset := token.NewFileSet()
		files, err := parseFiles(d.fs, fset, relpath, abspath, pkgfiles)
		if err != nil {
			info.Err = err
			return info
		}

		// ignore any errors - they are due to unresolved identifiers
		pkg, _ := ast.NewPackage(fset, files, poorMansImporter, nil)

		// extract package documentation
		info.FSet = fset
		if mode&ShowSource == 0 {
			// show extracted documentation
			var m doc.Mode
			if mode&NoFiltering != 0 {
				m |= doc.AllDecls
			}
			if mode&AllMethods != 0 {
				m |= doc.AllMethods
			}
			info.PDoc = doc.New(pkg, pathpkg.Clean(relpath), m) // no trailing '/' in importpath
			if mode&NoTypeAssoc != 0 {
				for _, t := range info.PDoc.Types {
					info.PDoc.Consts = append(info.PDoc.Consts, t.Consts...)
					info.PDoc.Vars = append(info.PDoc.Vars, t.Vars...)
					info.PDoc.Funcs = append(info.PDoc.Funcs, t.Funcs...)
					t.Consts = nil
					t.Vars = nil
					t.Funcs = nil
				}
				// for now we cannot easily sort consts and vars since
				// go/doc.Value doesn't export the order information
				sort.Sort(funcsByName(info.PDoc.Funcs))
			}

			// collect examples
			testfiles := append(pkginfo.TestGoFiles, pkginfo.XTestGoFiles...)
			files, err = parseFiles(d.fs, fset, relpath, abspath, testfiles)
			if err != nil {
				log.Println("parsing examples:", err)
			}
			info.Examples = collectExamples(pkg, files)
			info.Bugs = info.PDoc.Notes["BUG"]
		} else {
			// show source code
			// TODO(gri) Consider eliminating export filtering in this mode,
			//           or perhaps eliminating the mode altogether.
			if mode&NoFiltering == 0 {
				packageExports(fset, pkg)
			}
			info.PAst = files
		}
		info.IsMain = pkgname == "main"
	}

	info.Dirs = d.root.lookup(abspath).listing(func(path string) bool { return d.includePath(path, mode) })
	info.DirFlat = mode&FlatDir != 0

	return info
}

func (d *DocTree) includePath(path string, mode PageInfoMode) (r bool) {
	// if the path includes 'internal', don't list unless we are in the NoFiltering mode.
	if mode&NoFiltering != 0 {
		return true
	}
	if strings.Contains(path, "internal") || strings.Contains(path, "vendor") {
		for _, c := range strings.Split(filepath.Clean(path), string(os.PathSeparator)) {
			if c == "internal" || c == "vendor" {
				return false
			}
		}
	}
	return true
}

type funcsByName []*doc.Func

func (s funcsByName) Len() int           { return len(s) }
func (s funcsByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s funcsByName) Less(i, j int) bool { return s[i].Name < s[j].Name }

func (h *docServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if redirect(w, r) {
		return
	}

	// TODO(rsc): URL should be clean already.
	relpath := pathpkg.Clean(strings.TrimPrefix(r.URL.Path, "/pkg/"))

	abspath := pathpkg.Join("/src", relpath)
	mode := GetPageInfoMode(r.FormValue("m"))
	if relpath == "builtin" {
		// The fake built-in package contains unexported identifiers,
		// but we want to show them. Also, disable type association,
		// since it's not helpful for this fake package (see issue 6645).
		mode |= NoFiltering | NoTypeAssoc
	}
	info := h.d.GetPageInfo(abspath, relpath, mode, r.FormValue("GOOS"), r.FormValue("GOARCH"))
	if info.Err != nil {
		log.Print(info.Err)
		h.p.ServeError(w, r, relpath, info.Err)
		return
	}

	var tabtitle, title, subtitle string
	switch {
	case info.PAst != nil:
		for _, ast := range info.PAst {
			tabtitle = ast.Name.Name
			break
		}
	case info.PDoc != nil:
		tabtitle = info.PDoc.Name
	default:
		tabtitle = info.Dirname
		title = "Directory "
	}
	if title == "" {
		if info.IsMain {
			// assume that the directory name is the command name
			_, tabtitle = pathpkg.Split(relpath)
			title = "Command "
		} else {
			title = "Package "
		}
	}
	title += tabtitle

	// special cases for top-level package/command directories
	switch tabtitle {
	case "/src":
		title = "Packages"
		tabtitle = "Packages"
	case "/src/cmd":
		title = "Commands"
		tabtitle = "Commands"
	}

	info.GoogleCN = h.p.googleCN(r)
	var body []byte
	if info.Dirname == "/src" {
		body = applyTemplate(h.p.PackageRootHTML, "packageRootHTML", info)
	} else {
		body = applyTemplate(h.p.PackageHTML, "packageHTML", info)
	}
	h.p.ServePage(w, Page{
		Title:    title,
		Tabtitle: tabtitle,
		Subtitle: subtitle,
		Body:     body,
		GoogleCN: info.GoogleCN,
	})
}

type PageInfoMode uint

const (
	NoFiltering PageInfoMode = 1 << iota // do not filter exports
	FlatDir                              // show directory in a flat (non-indented) manner
	AllMethods                           // show all embedded methods
	ShowSource                           // show source code, do not extract documentation
	NoTypeAssoc                          // don't associate consts, vars, and factory functions with types (not exposed via ?m= query parameter, used for package builtin, see issue 6645)
)

// modeNames defines names for each PageInfoMode flag.
// The order here must match the order of the constants above.
var modeNames = []string{
	"all",
	"flat",
	"methods",
	"src",
}

// generate a query string for persisting PageInfoMode between pages.
func (m PageInfoMode) String() string {
	s := ""
	for i, name := range modeNames {
		if m&(1<<i) != 0 && name != "" {
			if s != "" {
				s += ","
			}
			s += name
		}
	}
	return s
}

func modeQueryString(m PageInfoMode) string {
	s := m.String()
	if s == "" {
		return ""
	}
	return "?m=" + s
}

// GetPageInfoMode computes the PageInfoMode flags by analyzing the request
// URL form value "m". It is value is a comma-separated list of mode names (for example, "all,flat").
func GetPageInfoMode(text string) PageInfoMode {
	var mode PageInfoMode
	for _, k := range strings.Split(text, ",") {
		k = strings.TrimSpace(k)
		for i, name := range modeNames {
			if name == k {
				mode |= 1 << i
			}
		}
	}
	return mode
}

// poorMansImporter returns a (dummy) package object named
// by the last path component of the provided package path
// (as is the convention for packages). This is sufficient
// to resolve package identifiers without doing an actual
// import. It never returns an error.
//
func poorMansImporter(imports map[string]*ast.Object, path string) (*ast.Object, error) {
	pkg := imports[path]
	if pkg == nil {
		// note that strings.LastIndex returns -1 if there is no "/"
		pkg = ast.NewObj(ast.Pkg, path[strings.LastIndex(path, "/")+1:])
		pkg.Data = ast.NewScope(nil) // required by ast.NewPackage for dot-import
		imports[path] = pkg
	}
	return pkg, nil
}

// globalNames returns a set of the names declared by all package-level
// declarations. Method names are returned in the form Receiver_Method.
func globalNames(pkg *ast.Package) map[string]bool {
	names := make(map[string]bool)
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			addNames(names, decl)
		}
	}
	return names
}

// collectExamples collects examples for pkg from testfiles.
func collectExamples(pkg *ast.Package, testfiles map[string]*ast.File) []*doc.Example {
	var files []*ast.File
	for _, f := range testfiles {
		files = append(files, f)
	}

	var examples []*doc.Example
	globals := globalNames(pkg)
	for _, e := range doc.Examples(files...) {
		name := stripExampleSuffix(e.Name)
		if name == "" || globals[name] {
			examples = append(examples, e)
		}
	}

	return examples
}

// addNames adds the names declared by decl to the names set.
// Method names are added in the form ReceiverTypeName_Method.
func addNames(names map[string]bool, decl ast.Decl) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		name := d.Name.Name
		if d.Recv != nil {
			var typeName string
			switch r := d.Recv.List[0].Type.(type) {
			case *ast.StarExpr:
				typeName = r.X.(*ast.Ident).Name
			case *ast.Ident:
				typeName = r.Name
			}
			name = typeName + "_" + name
		}
		names[name] = true
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names[s.Name.Name] = true
			case *ast.ValueSpec:
				for _, id := range s.Names {
					names[id.Name] = true
				}
			}
		}
	}
}

// packageExports is a local implementation of ast.PackageExports
// which correctly updates each package file's comment list.
// (The ast.PackageExports signature is frozen, hence the local
// implementation).
//
func packageExports(fset *token.FileSet, pkg *ast.Package) {
	for _, src := range pkg.Files {
		cmap := ast.NewCommentMap(fset, src, src.Comments)
		ast.FileExports(src)
		src.Comments = cmap.Filter(src).Comments()
	}
}

func applyTemplate(t *template.Template, name string, data interface{}) []byte {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Printf("%s.Execute: %s", name, err)
	}
	return buf.Bytes()
}

type writerCapturesErr struct {
	w   io.Writer
	err error
}

func (w *writerCapturesErr) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// applyTemplateToResponseWriter uses an http.ResponseWriter as the io.Writer
// for the call to template.Execute.  It uses an io.Writer wrapper to capture
// errors from the underlying http.ResponseWriter.  Errors are logged only when
// they come from the template processing and not the Writer; this avoid
// polluting log files with error messages due to networking issues, such as
// client disconnects and http HEAD protocol violations.
func applyTemplateToResponseWriter(rw http.ResponseWriter, t *template.Template, data interface{}) {
	w := &writerCapturesErr{w: rw}
	err := t.Execute(w, data)
	// There are some cases where template.Execute does not return an error when
	// rw returns an error, and some where it does.  So check w.err first.
	if w.err == nil && err != nil {
		// Log template errors.
		log.Printf("%s.Execute: %s", t.Name(), err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request) (redirected bool) {
	canonical := pathpkg.Clean(r.URL.Path)
	if !strings.HasSuffix(canonical, "/") {
		canonical += "/"
	}
	if r.URL.Path != canonical {
		url := *r.URL
		url.Path = canonical
		http.Redirect(w, r, url.String(), http.StatusMovedPermanently)
		redirected = true
	}
	return
}

func redirectFile(w http.ResponseWriter, r *http.Request) (redirected bool) {
	c := pathpkg.Clean(r.URL.Path)
	c = strings.TrimRight(c, "/")
	if r.URL.Path != c {
		url := *r.URL
		url.Path = c
		http.Redirect(w, r, url.String(), http.StatusMovedPermanently)
		redirected = true
	}
	return
}

var selRx = regexp.MustCompile(`^([0-9]+):([0-9]+)`)

// rangeSelection computes the Selection for a text range described
// by the argument str, of the form Start:End, where Start and End
// are decimal byte offsets.
func rangeSelection(str string) texthtml.Selection {
	m := selRx.FindStringSubmatch(str)
	if len(m) >= 2 {
		from, _ := strconv.Atoi(m[1])
		to, _ := strconv.Atoi(m[2])
		if from < to {
			return texthtml.Spans(texthtml.Span{Start: from, End: to})
		}
	}
	return nil
}

func (p *Presentation) serveTextFile(w http.ResponseWriter, r *http.Request, abspath, relpath, title string) {
	src, err := fs.ReadFile(p.Corpus.fs, toFS(abspath))
	if err != nil {
		log.Printf("ReadFile: %s", err)
		p.ServeError(w, r, relpath, err)
		return
	}

	if r.FormValue("m") == "text" {
		p.ServeText(w, src)
		return
	}

	cfg := texthtml.Config{
		GoComments: pathpkg.Ext(abspath) == ".go",
		Highlight:  r.FormValue("h"),
		Selection:  rangeSelection(r.FormValue("s")),
		Line:       1,
	}

	var buf bytes.Buffer
	buf.WriteString("<pre>")
	buf.Write(texthtml.Format(src, cfg))
	buf.WriteString("</pre>")

	fmt.Fprintf(&buf, `<p><a href="/%s?m=text">View as plain text</a></p>`, htmlpkg.EscapeString(relpath))

	p.ServePage(w, Page{
		Title:    title,
		SrcPath:  relpath,
		Tabtitle: relpath,
		Body:     buf.Bytes(),
		GoogleCN: p.googleCN(r),
	})
}

func (p *Presentation) serveDirectory(w http.ResponseWriter, r *http.Request, abspath, relpath string) {
	if redirect(w, r) {
		return
	}

	list, err := fs.ReadDir(p.Corpus.fs, toFS(abspath))
	if err != nil {
		p.ServeError(w, r, relpath, err)
		return
	}

	var info []fs.FileInfo
	for _, d := range list {
		i, err := d.Info()
		if err == nil {
			info = append(info, i)
		}
	}

	p.ServePage(w, Page{
		Title:    "Directory",
		SrcPath:  relpath,
		Tabtitle: relpath,
		Body:     applyTemplate(p.DirlistHTML, "dirlistHTML", info),
		GoogleCN: p.googleCN(r),
	})
}

func (p *Presentation) ServeHTMLDoc(w http.ResponseWriter, r *http.Request, abspath, relpath string) {
	// get HTML body contents
	isMarkdown := false
	src, err := fs.ReadFile(p.Corpus.fs, toFS(abspath))
	if err != nil && strings.HasSuffix(abspath, ".html") {
		if md, errMD := fs.ReadFile(p.Corpus.fs, toFS(strings.TrimSuffix(abspath, ".html")+".md")); errMD == nil {
			src = md
			isMarkdown = true
			err = nil
		}
	}
	if err != nil {
		log.Printf("ReadFile: %s", err)
		p.ServeError(w, r, relpath, err)
		return
	}

	// if it begins with "<!DOCTYPE " assume it is standalone
	// html that doesn't need the template wrapping.
	if bytes.HasPrefix(src, doctype) {
		w.Write(src)
		return
	}

	// if it begins with a JSON blob, read in the metadata.
	meta, src, err := extractMetadata(src)
	if err != nil {
		log.Printf("decoding metadata %s: %v", relpath, err)
	}

	page := Page{
		Title:    meta.Title,
		Subtitle: meta.Subtitle,
		GoogleCN: p.googleCN(r),
	}

	// evaluate as template if indicated
	if meta.Template {
		tmpl, err := template.New("main").Funcs(p.TemplateFuncs()).Parse(string(src))
		if err != nil {
			log.Printf("parsing template %s: %v", relpath, err)
			p.ServeError(w, r, relpath, err)
			return
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, page); err != nil {
			log.Printf("executing template %s: %v", relpath, err)
			p.ServeError(w, r, relpath, err)
			return
		}
		src = buf.Bytes()
	}

	// Apply markdown as indicated.
	// (Note template applies before Markdown.)
	if isMarkdown {
		html, err := renderMarkdown(src)
		if err != nil {
			log.Printf("executing markdown %s: %v", relpath, err)
			p.ServeError(w, r, relpath, err)
			return
		}
		src = html
	}

	// if it's the language spec, add tags to EBNF productions
	if strings.HasSuffix(abspath, "go_spec.html") {
		var buf bytes.Buffer
		spec.Linkify(&buf, src)
		src = buf.Bytes()
	}

	page.Body = src
	p.ServePage(w, page)
}

func (p *Presentation) ServeFile(w http.ResponseWriter, r *http.Request) {
	p.serveFile(w, r)
}

func (p *Presentation) serveFile(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/index.html") {
		// We'll show index.html for the directory.
		// Use the dir/ version as canonical instead of dir/index.html.
		http.Redirect(w, r, r.URL.Path[0:len(r.URL.Path)-len("index.html")], http.StatusMovedPermanently)
		return
	}

	// Check to see if we need to redirect or serve another file.
	relpath := r.URL.Path
	if m := p.Corpus.MetadataFor(relpath); m != nil {
		if m.Path != relpath {
			// Redirect to canonical path.
			http.Redirect(w, r, m.Path, http.StatusMovedPermanently)
			return
		}
		// Serve from the actual filesystem path.
		relpath = m.filePath
	}

	abspath := relpath
	relpath = relpath[1:] // strip leading slash

	switch pathpkg.Ext(relpath) {
	case ".html":
		p.ServeHTMLDoc(w, r, abspath, relpath)
		return

	case ".go":
		p.serveTextFile(w, r, abspath, relpath, "Source file")
		return
	}

	dir, err := fs.Stat(p.Corpus.fs, toFS(abspath))
	if err != nil {
		log.Print(err)
		p.ServeError(w, r, relpath, err)
		return
	}

	fsPath := toFS(abspath)
	if dir != nil && dir.IsDir() {
		if redirect(w, r) {
			return
		}
		index := pathpkg.Join(fsPath, "index.html")
		if isTextFile(p.Corpus.fs, index) || isTextFile(p.Corpus.fs, pathpkg.Join(fsPath, "index.md")) {
			p.ServeHTMLDoc(w, r, index, index)
			return
		}
		p.serveDirectory(w, r, abspath, relpath)
		return
	}

	if isTextFile(p.Corpus.fs, fsPath) {
		if redirectFile(w, r) {
			return
		}
		p.serveTextFile(w, r, abspath, relpath, "Text file")
		return
	}

	p.fileServer.ServeHTTP(w, r)
}

func (p *Presentation) ServeText(w http.ResponseWriter, text []byte) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(text)
}

func marshalJSON(x interface{}) []byte {
	var data []byte
	var err error
	const indentJSON = false // for easier debugging
	if indentJSON {
		data, err = json.MarshalIndent(x, "", "    ")
	} else {
		data, err = json.Marshal(x)
	}
	if err != nil {
		panic(fmt.Sprintf("json.Marshal failed: %s", err))
	}
	return data
}