package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// A one-billion-parameter model cannot be trusted with arithmetic, so the calculator tool does it
// exactly. It reads + - * / % ^ (power), parentheses, decimals, and the functions sqrt, abs and round.

func evalExpression(input string) (float64, error) {
	p := &exprParser{src: strings.NewReplacer("×", "*", "÷", "/", "−", "-", ",", "").Replace(input)}
	value, err := p.sum()
	if err != nil {
		return 0, err
	}
	p.space()
	if p.pos < len(p.src) {
		return 0, fmt.Errorf("unexpected %q", p.src[p.pos:])
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("the result is not a number")
	}
	return value, nil
}

type exprParser struct {
	src   string
	pos   int
	depth int
}

func (p *exprParser) space() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *exprParser) eat(c byte) bool {
	p.space()
	if p.pos < len(p.src) && p.src[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

func (p *exprParser) sum() (float64, error) {
	left, err := p.product()
	if err != nil {
		return 0, err
	}
	for {
		switch {
		case p.eat('+'):
			right, err := p.product()
			if err != nil {
				return 0, err
			}
			left += right
		case p.eat('-'):
			right, err := p.product()
			if err != nil {
				return 0, err
			}
			left -= right
		default:
			return left, nil
		}
	}
}

func (p *exprParser) product() (float64, error) {
	left, err := p.power()
	if err != nil {
		return 0, err
	}
	for {
		switch {
		case p.eat('*'), p.eat('x'), p.eat('X'):
			right, err := p.power()
			if err != nil {
				return 0, err
			}
			left *= right
		case p.eat('/'):
			right, err := p.power()
			if err != nil {
				return 0, err
			}
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			left /= right
		case p.eat('%'):
			right, err := p.power()
			if err != nil {
				return 0, err
			}
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			left = math.Mod(left, right)
		default:
			return left, nil
		}
	}
}

func (p *exprParser) power() (float64, error) {
	base, err := p.unary()
	if err != nil {
		return 0, err
	}
	if p.eat('^') {
		exp, err := p.power() // right-associative
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *exprParser) unary() (float64, error) {
	if p.eat('-') {
		v, err := p.unary()
		return -v, err
	}
	if p.eat('+') {
		return p.unary()
	}
	return p.atom()
}

func (p *exprParser) atom() (float64, error) {
	p.space()
	if p.depth > 50 {
		return 0, errors.New("too deeply nested")
	}
	if p.eat('(') {
		p.depth++
		v, err := p.sum()
		p.depth--
		if err != nil {
			return 0, err
		}
		if !p.eat(')') {
			return 0, errors.New("missing )")
		}
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' || p.src[p.pos] == '.') {
		p.pos++
	}
	if p.pos > start {
		return strconv.ParseFloat(p.src[start:p.pos], 64)
	}
	for p.pos < len(p.src) && p.src[p.pos] >= 'a' && p.src[p.pos] <= 'z' {
		p.pos++
	}
	name := p.src[start:p.pos]
	if name == "" {
		if p.pos < len(p.src) {
			return 0, fmt.Errorf("unexpected %q", p.src[p.pos:])
		}
		return 0, errors.New("the expression ended early")
	}
	if !p.eat('(') {
		return 0, fmt.Errorf("unknown word %q", name)
	}
	p.depth++
	v, err := p.sum()
	p.depth--
	if err != nil {
		return 0, err
	}
	if !p.eat(')') {
		return 0, errors.New("missing )")
	}
	switch name {
	case "sqrt":
		if v < 0 {
			return 0, errors.New("square root of a negative number")
		}
		return math.Sqrt(v), nil
	case "abs":
		return math.Abs(v), nil
	case "round":
		return math.Round(v), nil
	}
	return 0, fmt.Errorf("unknown function %q", name)
}

func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 12, 64)
}
