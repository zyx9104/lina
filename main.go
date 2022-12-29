package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/fatih/structtag"
	"golang.org/x/tools/go/packages"
)

const (
	GetTag  = "r"
	SetTag  = "w"
	SkipTag = "skip"
	TagName = "lina"
	LockTag = "lock"
)

var (
	typeNames  = flag.String("type", "", "comma-separated list of type names; must be set")
	output     = ""
	getterTmpl = "getter"
	setterTmpl = "setter"
	tmpl       = map[string]string{
		"setter": `func ({{.Receiver}} *{{.Struct}}) DoNotUseThisToSet{{.UpperField}}({{.Field}} {{.Type}}) {
	{{.Receiver}}.{{.Field}} = {{.Field}}
}
`,
		"getter": `func ({{.Receiver}} *{{.Struct}}) {{.UpperField}}() {{.Type}} {
	{{.Receiver}}.RLock()
	defer {{.Receiver}}.RUnlock()
	return {{.Receiver}}.{{.Field}}
}
`,
	}
)

var (
	SETTER = true
	GETTER = true
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of lina:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(log.Prefix())
	flag.Usage = Usage
	flag.Parse()

	if len(*typeNames) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	types := strings.Split(*typeNames, ",")

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	// var dir string
	g := Generator{
		buf:      make(map[string]*bytes.Buffer),
		walkMark: make(map[string]bool),
	}

	g.parsePackage(args)

	// Print the header and package clause.
	// Run generate for each type.
	for i, typeName := range types {
		g.generate(typeName)
		// AccessWrite to file.
		if output == "" {
			output = fmt.Sprintf("%s_lina.go", types[i])
		}
		outputName := filepath.Join(".", strings.ToLower(output))
		buf := g.buf[typeName]
		var src = (buf).Bytes()

		err := os.WriteFile(outputName, src, 0644)
		if err != nil {
			log.Fatalf("writing output: %s", err)
		}
	}

}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf      map[string]*bytes.Buffer // Accumulated output.
	pkg      *Package                 // Package we are scanning.
	walkMark map[string]bool
}

func (g *Generator) Printf(structName, format string, args ...interface{}) {
	buf, ok := g.buf[structName]
	if !ok {
		buf = bytes.NewBufferString("")
		g.buf[structName] = buf
	}
	fmt.Fprintf(buf, format, args...)
}

// File holds a single parsed file and associated data.
type File struct {
	pkg     *Package  // Package to which this file belongs.
	file    *ast.File // Parsed AST.
	fileSet *token.FileSet
	// These fields are reset for each type being generated.
	typeName string // Name of the constant type.

}

type Package struct {
	name  string
	defs  map[*ast.Ident]types.Object
	files []*File
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *Generator) parsePackage(patterns []string) {
	mode := packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo
	cfg := &packages.Config{
		Mode:  mode,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *Generator) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name:  pkg.Name,
		defs:  pkg.TypesInfo.Defs,
		files: make([]*File, len(pkg.Syntax)),
	}

	for i, file := range pkg.Syntax {
		g.pkg.files[i] = &File{
			file:    file,
			pkg:     g.pkg,
			fileSet: pkg.Fset,
		}
	}
}

// generate produces the String method for the named type.
func (g *Generator) generate(typeName string) {
	for _, file := range g.pkg.files {
		// Set the state for this run of the walker.
		file.typeName = typeName
		//ast.Print(file.fileSet, file.file)
		if file.file != nil {

			structInfo, err := ParseStruct(file.file, file.fileSet, TagName)
			if err != nil {
				log.Panic(err)
			}

			for stName, info := range structInfo {
				if stName != typeName {
					continue
				}
				g.Printf(stName, "package %s\n", g.pkg.name)
				g.Printf(stName, "\n")

				for _, field := range info {
					for _, access := range field.Access {
						switch access {
						case SetTag:
							if SETTER {
								g.Printf(stName, "%s\n", genSetter(stName, field.Name, field.Type))
							}
						case GetTag:
							if GETTER {
								g.Printf(stName, "%s\n", genGetter(stName, field.Name, field.Type))
							}

						case SkipTag:
							continue
						default:
							log.Fatalf("unknow tag %q in field %q struct %q", access, field.Name, stName)
						}
					}

				}
			}

		}
	}

}

type StructFieldInfo struct {
	Name   string
	Type   string
	Access []string
}
type StructFieldInfoArr = []StructFieldInfo

func ParseStruct(file *ast.File, fileSet *token.FileSet, tagName string) (structMap map[string]StructFieldInfoArr, err error) {
	structMap = make(map[string]StructFieldInfoArr)

	collectStructs := func(x ast.Node) bool {
		ts, ok := x.(*ast.TypeSpec)
		if !ok || ts.Type == nil {
			return true
		}

		structName := ts.Name.Name

		s, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		fileInfos := make([]StructFieldInfo, 0)

		for _, field := range s.Fields.List {
			if len(field.Names) == 0 {
				continue
			}
			name := field.Names[0].Name
			info := StructFieldInfo{Name: name}
			var typeNameBuf bytes.Buffer
			err := printer.Fprint(&typeNameBuf, fileSet, field.Type)
			if err != nil {
				log.Println("get type failed:", err)
				return true
			}
			info.Type = typeNameBuf.String()
			if field.Tag != nil {
				tag := field.Tag.Value
				tag = strings.Trim(tag, "`")
				tags, err := structtag.Parse(tag)
				if err != nil {
					return true
				}
				access, err := tags.Get(tagName)
				if err != nil {
					info.Access = []string{GetTag, SetTag}
				} else {
					access.Options = append(access.Options, access.Name)
					info.Access = access.Options
				}
			} else if name == "mutex" {
				info.Access = []string{LockTag}
			} else if !token.IsExported(name) {
				info.Access = []string{GetTag, SetTag}
			}
			fileInfos = append(fileInfos, info)
		}
		structMap[structName] = fileInfos
		return false
	}

	ast.Inspect(file, collectStructs)

	return structMap, nil
}

func genGetter(structName, fieldName, typeName string) string {
	return genFunc(getterTmpl, structName, fieldName, typeName, "")
}
func genSetter(structName, fieldName, typeName string) string {
	return genFunc(setterTmpl, structName, fieldName, typeName, "")
}

func genFunc(funcName, structName, fieldName, typeName, lockName string) string {
	t := template.New(funcName)
	t = template.Must(t.Parse(tmpl[funcName]))
	res := bytes.NewBufferString("")
	upperName := fmt.Sprintf("%s%s", strings.ToUpper(fieldName[0:1]), fieldName[1:])
	t.Execute(res, map[string]string{
		"Receiver":   strings.ToLower(structName[0:1]),
		"Struct":     structName,
		"Field":      fieldName,
		"Type":       typeName,
		"UpperField": upperName,
		"Lock":       lockName,
	})
	return res.String()
}
