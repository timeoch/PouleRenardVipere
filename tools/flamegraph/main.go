// flamegraph dessine un flamegraph SVG annoté à partir d'un profil pprof réel,
// via la sortie texte de « go tool pprof -traces » (pile complète de chaque
// échantillon). Racine en haut, comme la vue Flame Graph de pprof ; largeur
// d'un cadre = part du temps (ou des octets) de l'échantillon.
//
//	go tool pprof -traces prv.test.exe cpu.pprof > cpu.traces
//	go run ./tools/flamegraph -in cpu.traces -out cpu.svg -focus benchTicks \
//	    -note 'Nearest=Recherche du plus proche' -title 'V0 : profil CPU'
//
// -note (répétable) entoure en rouge les cadres dont le nom correspond à
// l'expression et les numérote ; la légende sous le graphe donne leur part.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type node struct {
	name     string
	value    float64
	children map[string]*node
	order    []string
}

func (n *node) child(name string) *node {
	if c, ok := n.children[name]; ok {
		return c
	}
	c := &node{name: name, children: map[string]*node{}}
	n.children[name] = c
	n.order = append(n.order, name)
	return c
}

type note struct {
	re    *regexp.Regexp
	label string
	value float64 // somme des cadres annotés les plus hauts (sans double compte)
}

type notes []*note

func (ns *notes) String() string { return "" }
func (ns *notes) Set(v string) error {
	k := strings.Index(v, "=")
	if k < 0 {
		return fmt.Errorf("attendu regex=texte : %q", v)
	}
	re, err := regexp.Compile(v[:k])
	if err != nil {
		return err
	}
	*ns = append(*ns, &note{re: re, label: v[k+1:]})
	return nil
}

var units = map[string]float64{
	"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1, "min": 60, "hrs": 3600,
	"B": 1, "kB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40,
}

var (
	valueRe = regexp.MustCompile(`^\s*([0-9.]+)([a-zµA-Z]*)\s+(.*)$`)
	labelRe = regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z_ ]*:\s`)
)

func parseValue(num, unit string) (float64, bool, error) {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, false, err
	}
	if unit == "" {
		return v, false, nil
	}
	m, ok := units[unit]
	if !ok {
		return 0, false, fmt.Errorf("unité inconnue %q", unit)
	}
	_, isTime := map[string]bool{"ns": true, "us": true, "µs": true, "ms": true, "s": true, "min": true, "hrs": true}[unit]
	return v * m, isTime, nil
}

func main() {
	in := flag.String("in", "", "sortie de go tool pprof -traces (stdin si vide)")
	out := flag.String("out", "flame.svg", "fichier SVG produit")
	title := flag.String("title", "Flamegraph", "titre")
	subtitle := flag.String("subtitle", "", "sous-titre (machine, commande…)")
	focus := flag.String("focus", "", "ne garder que les piles passant par ce cadre, qui devient la racine")
	width := flag.Float64("width", 1400, "largeur en pixels")
	minPct := flag.Float64("min", 0.15, "largeur minimale d'un cadre dessiné (% du total)")
	maxDepth := flag.Int("depth", 0, "profondeur maximale dessinée (0 : illimitée) ; les parts annotées restent calculées sur tout le profil")
	var ns notes
	flag.Var(&ns, "note", "regex=texte : cadre à annoter (répétable)")
	flag.Parse()

	var r io.Reader = os.Stdin
	if *in != "" {
		f, err := os.Open(*in)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		r = f
	}
	var focusRe *regexp.Regexp
	if *focus != "" {
		focusRe = regexp.MustCompile(*focus)
	}

	root := &node{name: "total", children: map[string]*node{}}
	isTime := true
	var stack []string
	var val float64
	flush := func() {
		if len(stack) == 0 {
			return
		}
		// stack : feuille d'abord. On la retourne pour partir de la racine.
		frames := make([]string, 0, len(stack))
		for i := len(stack) - 1; i >= 0; i-- {
			frames = append(frames, stack[i])
		}
		if focusRe != nil {
			k := -1
			for i, f := range frames {
				if focusRe.MatchString(f) {
					k = i
					break
				}
			}
			if k < 0 {
				stack = stack[:0]
				return
			}
			frames = frames[k:]
		}
		n := root
		n.value += val
		for _, f := range frames {
			n = n.child(f)
			n.value += val
		}
		stack = stack[:0]
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	inBody := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "-----------+") {
			flush()
			inBody = true
			continue
		}
		if !inBody || strings.TrimSpace(line) == "" || labelRe.MatchString(line) {
			continue // ligne vide ou étiquette d'échantillon (« bytes: 320kB » des profils mémoire)
		}
		if m := valueRe.FindStringSubmatch(line); m != nil && len(stack) == 0 {
			v, t, err := parseValue(m[1], m[2])
			if err != nil {
				fail(err)
			}
			val, isTime = v, t
			stack = append(stack, clean(m[3]))
			continue
		}
		stack = append(stack, clean(strings.TrimSpace(line)))
	}
	flush()
	if root.value == 0 {
		fail(fmt.Errorf("aucun échantillon (focus %q ?)", *focus))
	}
	if focusRe != nil && len(root.order) == 1 {
		root = root.children[root.order[0]] // le cadre de focus devient la racine
	}

	// Annotations : chaque cadre annoté est compté une fois, sans ses
	// descendants qui correspondraient aussi (récursion, appels imbriqués).
	var mark func(n *node, inside map[*note]bool)
	mark = func(n *node, inside map[*note]bool) {
		next := inside
		copied := false
		for _, nt := range ns {
			if inside[nt] || !nt.re.MatchString(n.name) {
				continue
			}
			nt.value += n.value
			if !copied {
				next = make(map[*note]bool, len(inside)+1)
				for k, v := range inside {
					next[k] = v
				}
				copied = true
			}
			next[nt] = true
		}
		for _, k := range n.order {
			mark(n.children[k], next)
		}
	}
	mark(root, map[*note]bool{})

	svg := render(root, ns, *title, *subtitle, *width, *minPct, *maxDepth, isTime)
	if err := os.WriteFile(*out, []byte(svg), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("%s : total %s\n", *out, human(root.value, isTime))
}

// fr formate un nombre à la française (virgule décimale).
func fr(format string, v float64) string {
	return strings.Replace(fmt.Sprintf(format, v), ".", ",", 1)
}

// clean retire le chemin du module et l'indication (inline) des noms.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "flamegraph :", err)
	os.Exit(1)
}

func clean(s string) string {
	s = strings.TrimSuffix(s, " (inline)")
	s = strings.TrimPrefix(s, "poule-renard-vipere.")
	return s
}

func human(v float64, isTime bool) string {
	if isTime {
		if v >= 1 {
			return fr("%.2f s", v)
		}
		return fr("%.0f ms", v*1000)
	}
	switch {
	case v >= 1<<30:
		return fr("%.2f Go", v/(1<<30))
	case v >= 1<<20:
		return fr("%.1f Mo", v/(1<<20))
	default:
		return fr("%.0f Ko", v/(1<<10))
	}
}

const (
	rowH    = 18.0
	padX    = 10.0
	headerH = 58.0
	charW   = 6.7
)

var stdlib = map[string]bool{"math": true, "sync": true, "testing": true, "time": true, "syscall": true,
	"os": true, "fmt": true, "strconv": true, "sort": true, "bytes": true, "bufio": true, "reflect": true,
	"unicode": true, "slices": true, "maps": true, "errors": true, "context": true, "io": true,
	"container": true, "strings": true, "net": true, "crypto": true}

// color : bleu pour le runtime Go (GC, allocateur, ordonnanceur), vert pour la
// bibliothèque standard et les dépendances, orange pour le code du jeu (dont
// clean a retiré le chemin de module).
func color(name string) string {
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	j := (h & 0xff) % 40
	pkg := name
	if k := strings.IndexAny(name, ".("); k >= 0 {
		pkg = name[:k]
	}
	switch {
	case strings.HasPrefix(name, "runtime.") || strings.HasPrefix(name, "internal/"):
		return fmt.Sprintf("rgb(%d,%d,%d)", 110+j/2, 160+j/2, 225)
	case strings.Contains(pkg, "/") || stdlib[pkg]:
		return fmt.Sprintf("rgb(%d,%d,%d)", 150+j, 200+j/2, 125+j/2)
	}
	return fmt.Sprintf("rgb(%d,%d,%d)", 230+j/3, 120+j*2, 45+j)
}

func render(root *node, ns notes, title, subtitle string, width, minPct float64, maxDepth int, isTime bool) string {
	tooDeep := func(d int) bool { return maxDepth > 0 && d >= maxDepth }
	depth := 0
	var measure func(n *node, d int)
	measure = func(n *node, d int) {
		if n.value/root.value*100 < minPct || tooDeep(d) {
			return
		}
		depth = max(depth, d)
		for _, k := range n.order {
			measure(n.children[k], d+1)
		}
	}
	measure(root, 0)
	graphH := float64(depth+1) * rowH
	legendH := 26.0 + 20*float64(len(ns))
	height := headerH + graphH + legendH + 10
	scale := (width - 2*padX) / root.value

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="Consolas, 'DejaVu Sans Mono', monospace">`+"\n",
		width, height, width, height)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="#ffffff"/>`+"\n")
	fmt.Fprintf(&b, `<text x="%.0f" y="24" font-size="17" font-weight="bold" fill="#222">%s</text>`+"\n", padX, html.EscapeString(title))
	sub := subtitle
	if sub != "" {
		sub += ". "
	}
	sub += "Total " + human(root.value, isTime) + ". En bleu le runtime Go, en orange le code du jeu, en vert la bibliothèque standard."
	fmt.Fprintf(&b, `<text x="%.0f" y="44" font-size="12" fill="#555">%s</text>`+"\n", padX, html.EscapeString(sub))

	type callout struct {
		x, y float64
		k    int
	}
	var callouts []callout
	noteIdx := map[*note]int{}
	for i, nt := range ns {
		noteIdx[nt] = i + 1
	}
	seen := map[int]bool{}

	var draw func(n *node, x float64, d int, path string)
	draw = func(n *node, x float64, d int, path string) {
		w := n.value * scale
		if n.value/root.value*100 < minPct || tooDeep(d) {
			return
		}
		y := headerH + float64(d)*rowH
		pct := 100 * n.value / root.value
		tip := fmt.Sprintf("%s, %s (%s %%)", n.name, human(n.value, isTime), fr("%.1f", pct))
		fmt.Fprintf(&b, `<g><title>%s</title><rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s" stroke="#fff" stroke-width="0.6" rx="1.5"/>`,
			html.EscapeString(tip), x, y, w, rowH-1, color(n.name))
		if label := fit(n.name, pct, w); label != "" {
			fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="11" fill="#1a1a1a">%s</text>`, x+3, y+12.5, html.EscapeString(label))
		}
		b.WriteString("</g>\n")
		for _, nt := range ns {
			if nt.re.MatchString(n.name) {
				fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="none" stroke="#d00" stroke-width="2.2" rx="2"/>`+"\n",
					x+0.5, y+0.5, max(w-1, 2), rowH-2)
				k := noteIdx[nt]
				if !seen[k] && w > 14 {
					seen[k] = true
					callouts = append(callouts, callout{x + w - 10, y + rowH/2, k}) // à droite : le nom reste lisible
				}
			}
		}
		cx := x
		kids := append([]string(nil), n.order...)
		sort.SliceStable(kids, func(i, j int) bool { return n.children[kids[i]].value > n.children[kids[j]].value })
		for _, k := range kids {
			c := n.children[k]
			draw(c, cx, d+1, path+"/"+k)
			cx += c.value * scale
		}
	}
	draw(root, padX, 0, "")

	for _, c := range callouts {
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="8" fill="#d00"/><text x="%.1f" y="%.1f" font-size="11" font-weight="bold" fill="#fff" text-anchor="middle">%d</text>`+"\n",
			c.x, c.y, c.x, c.y+4, c.k)
	}

	ly := headerH + graphH + 22
	for i, nt := range ns {
		pct := 100 * nt.value / root.value
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="8" fill="#d00"/><text x="%.1f" y="%.1f" font-size="11" font-weight="bold" fill="#fff" text-anchor="middle">%d</text>`,
			padX+8, ly-4, padX+8, ly, i+1)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12.5" fill="#222"><tspan font-weight="bold">%s %%</tspan> (%s) %s</text>`+"\n",
			padX+22, ly, fr("%.1f", pct), human(nt.value, isTime), html.EscapeString(nt.label))
		ly += 20
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// fit raccourcit le nom pour qu'il tienne dans le cadre, avec sa part si la
// place le permet.
func fit(name string, pct, w float64) string {
	full := fmt.Sprintf("%s %s %%", name, fr("%.1f", pct))
	if float64(len(full))*charW+6 <= w {
		return full
	}
	if float64(len(name))*charW+6 <= w {
		return name
	}
	n := int((w - 6) / charW)
	if n < 4 {
		return ""
	}
	return name[:n-1] + "…"
}
