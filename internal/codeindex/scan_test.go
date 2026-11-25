package codeindex

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// syms renders symbols as "kind Parent.Name start-end" for comparison.
func syms(p Parsed) []string {
	var out []string
	for _, s := range p.Symbols {
		out = append(out, fmt.Sprintf("%s %s %d-%d", s.Kind, s.Qualified(), s.Start, s.End))
	}
	return out
}

func checkSyms(t *testing.T, lang, src string, want []string, wantImports ...string) {
	t.Helper()
	p := Scan(lang, []byte(src))
	got := syms(p)
	if !slices.Equal(got, want) {
		t.Errorf("%s symbols:\n got  %q\n want %q", lang, got, want)
	}
	if wantImports != nil && !slices.Equal(p.Imports, wantImports) {
		t.Errorf("%s imports: got %q want %q", lang, p.Imports, wantImports)
	}
}

func TestScanGo(t *testing.T) {
	src := `package x

import (
	"fmt"
	str "strings"
)

// Service does things.
// More text.
type Service struct {
	Name string
	n    int
}

type Doer interface{ Do() }

const A, B = 1, 2

var (
	// c is private
	c = 3
)

// Run runs it.
func (s *Service) Run(ctx int) error {
	return nil
}

func gen[T any](v T) T { return v }

func (g *Box[T]) Get() T {
	return g.v
}
`
	p := Scan("go", []byte(src))
	want := []string{
		"struct Service 8-13", "field Service.Name 11-11", "field Service.n 12-12",
		"interface Doer 15-15", "const A 17-17", "const B 17-17", "var c 20-21",
		"method Service.Run 24-27", "func gen 29-29", "method Box.Get 31-33",
	}
	if got := syms(p); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if p.Symbols[0].Doc != "Service does things." {
		t.Errorf("doc = %q", p.Symbols[0].Doc)
	}
	if p.Symbols[7].Sig != "func (s *Service) Run(ctx int) error" {
		t.Errorf("sig = %q", p.Symbols[7].Sig)
	}
	if !slices.Equal(p.Imports, []string{"fmt", "strings"}) {
		t.Errorf("imports = %q", p.Imports)
	}
	bad := Scan("go", []byte("package x\n\nfunc ok() {}\n\nfunc broken( {\n"))
	if len(bad.Symbols) == 0 || bad.Symbols[0].Name != "ok" {
		t.Errorf("broken go: %v", syms(bad))
	}
}

func TestScanPython(t *testing.T) {
	src := `import os, sys as s
from .models import Thing
from a.b import (c,
    d)

MAX_SIZE = 10
_private = 1

@decorator
class Foo(Base):
    """Foo does a thing.

    def not_a_def(self):
        pass
    """
    x = 1

    def method(self,
               a: int = 5) -> None:
        '''doc here'''
        s = """
def fake():
"""
        return a

    @property
    async def prop(self):
        def inner():
            pass
        return 1


def top(a, b):
    # comment at col 0 below
# stray comment
    return a + b
`
	checkSyms(t, "python", src, []string{
		"const MAX_SIZE 6-6", "var _private 7-7",
		"class Foo 9-30",
		"method Foo.method 18-24",
		"method Foo.prop 26-30", "func Foo.prop.inner 28-29",
		"func top 33-36",
	}, "os", "sys", ".models", "a.b")
	p := Scan("python", []byte(src))
	for _, s := range p.Symbols {
		if s.Name == "Foo" && s.Doc != "Foo does a thing." {
			t.Errorf("class doc = %q", s.Doc)
		}
		if s.Name == "method" && (s.Doc != "doc here" || !strings.HasPrefix(s.Sig, "def method(self, a: int = 5) -> None")) {
			t.Errorf("method doc/sig = %q / %q", s.Doc, s.Sig)
		}
	}
	for _, bad := range []string{"def (", "class", "'''unterminated", "x = (\n", "def f():\n\t\tpass\n  x"} {
		Scan("python", []byte(bad))
	}
}

func TestScanTypeScript(t *testing.T) {
	src := "import React, { useState } from 'react'\n" +
		"import type { A } from \"./types\"\n" +
		"export { b } from './b'\n" +
		"const lazy = require('lazy')\n" +
		"\n" +
		"/** Button props. */\n" +
		"export interface Props {\n" +
		"  label: string\n" +
		"  onClick(e: Event): void\n" +
		"}\n" +
		"\n" +
		"export type Id = string | number\n" +
		"\n" +
		"export const Button = ({ label }: Props) => {\n" +
		"  const re = /[/{]x}/g\n" +
		"  return `<b>${label}</b> {`\n" +
		"}\n" +
		"\n" +
		"export default class Widget extends Base {\n" +
		"  private count = 0\n" +
		"  // renders\n" +
		"  async render(a: string,\n" +
		"    b: number): Promise<void> {\n" +
		"    if (a) { return }\n" +
		"    const s = '}'\n" +
		"  }\n" +
		"  static make = () => new Widget()\n" +
		"  get size() { return 1 }\n" +
		"}\n" +
		"\n" +
		"export async function load(url: string): Promise<void> {\n" +
		"  await fetch(url)\n" +
		"}\n" +
		"const x = 1\n" +
		"let y = foo(\n" +
		"  1,\n" +
		")\n"
	checkSyms(t, "tsx", src, []string{
		"const lazy 4-4", "interface Props 6-10", "method Props.onClick 9-9", "type Id 12-12", "func Button 14-17",
		"class Widget 19-29", "method Widget.render 21-26", "method Widget.make 27-27", "method Widget.size 28-28",
		"func load 31-33", "const x 34-34", "var y 35-37",
	}, "react", "./types", "./b", "lazy")
}

func TestScanRust(t *testing.T) {
	src := `use std::collections::HashMap;
use crate::foo::{a, b};

/// A point.
#[derive(Debug)]
pub struct Point {
    x: i32, // brace } in comment
}

impl<T: Clone> Trait<T> for Point<T> {
    fn new() -> Self {
        let s = "}{";
        let c = '}';
        let l: &'static str = "x";
        Self {}
    }
    pub fn len(&self) -> usize { 1 }
}

pub(crate) async fn run<'a>(x: &'a str) -> Result<(), E>
where
    E: Debug,
{
    println!("{}", r#"raw } "quoted""#);
}

mod inner {
    pub fn deep() {}
}

const MAX: usize = 3;
macro_rules! mac { () => {} }
`
	checkSyms(t, "rust", src, []string{
		"type Point 4-8", "impl Point 10-18", "method Point.new 11-16", "method Point.len 17-17",
		"func run 20-25", "mod inner 27-29", "func inner.deep 28-28", "const MAX 31-31", "macro mac 32-32",
	}, "std::collections::HashMap", "crate::foo")
}

func TestScanJavaKotlin(t *testing.T) {
	java := `package a;

import java.util.List;
import static org.junit.Assert.*;

@Entity
public class Foo<T> extends Bar {
    private int x;

    @Override
    public Foo(int x) { this.x = x; }

    public static <T> List<T> make(
            int a,
            String b) throws Exception {
        if (a > 0) { return null; }
        return List.of("}");
    }

    void helper() {}
}
`
	checkSyms(t, "java", java, []string{
		"class Foo 6-21", "ctor Foo.Foo 10-11", "method Foo.make 13-18", "method Foo.helper 20-20",
	}, "java.util.List", "org.junit.Assert")
	kt := `package a
import a.b.C

data class P(val x: Int)

interface Repo {
    fun find(id: Int): P?
}

class Impl : Repo {
    override fun find(id: Int): P? = null
    private suspend fun load() {
        val s = """
        }
        """
    }
}

fun <T> List<T>.second(): T = this[1]
val TOP = 1
`
	checkSyms(t, "kotlin", kt, []string{
		"class P 4-4", "class Repo 6-8", "method Repo.find 7-7", "class Impl 10-17", "method Impl.find 11-11", "method Impl.load 12-16", "func second 19-19", "var TOP 20-20",
	}, "a.b.C")
}

func TestScanShell(t *testing.T) {
	src := `#!/bin/bash
source ./lib.sh

# Say hi.
hello() {
  echo "}"
}

function build {
  if x; then
    y
  fi
}

oneliner() { echo hi; }
echo done
`
	checkSyms(t, "shell", src, []string{"func hello 4-7", "func build 9-13", "func oneliner 15-15"}, "./lib.sh")
}

func TestScanSQL(t *testing.T) {
	src := `-- schema
CREATE TABLE IF NOT EXISTS users (
  id integer primary key, -- ; in comment
  name text
);

/* block
create table nope (x int);
*/
create unique index idx_users_name on users(name);
CREATE OR REPLACE FUNCTION f() RETURNS int AS $$
BEGIN
  RETURN 1;
END;
$$ LANGUAGE plpgsql;
ALTER TABLE users ADD COLUMN age int;
`
	checkSyms(t, "sql", src, []string{
		"table users 2-5", "index idx_users_name 10-10", "function f 11-15", "alter table users 16-16",
	})
}

func TestScanMarkdown(t *testing.T) {
	src := "---\ntitle: x\n---\n# Top\n\ntext\n\n```sh\n# not a heading\n```\n\n## Sub A\n\nx\n\n## Sub B\n\n### Deep\ny\n\n# Second\n"
	checkSyms(t, "markdown", src, []string{
		"heading Top 4-19", "heading Top.Sub A 12-14", "heading Top.Sub B 16-19", "heading Sub B.Deep 18-19", "heading Second 21-21",
	})
}

func TestScanYAML(t *testing.T) {
	src := `name: ci
on:
  push:
jobs:
  test:
    runs-on: x
    steps:
      - run: a
  lint:
    runs-on: y
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  labels:
    a: b
data:
  k: v
`
	got := syms(Scan("yaml", []byte(src)))
	want := []string{
		"key name 1-1", "key on 2-3", "key jobs 4-10", "key jobs.test 5-8", "key jobs.lint 9-10",
		"key apiVersion 12-12", "key kind 13-13", "key metadata 14-17", "key data 18-19", "configmap cfg 12-19",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestScanHTML(t *testing.T) {
	src := "{% extends \"base.html\" %}\n<title>Home page</title>\n{% block content %}\n<p>x</p>\n{% endblock %}\n"
	checkSyms(t, "html", src, []string{"title Home page 2-2", "block content 3-5"}, "base.html")
}

func TestScanJSWrappers(t *testing.T) {
	iife := "(function () {\n" +
		"  'use strict'\n" +
		"  function init(el) {\n" +
		"    el.focus()\n" +
		"  }\n" +
		"  const state = { open: false }\n" +
		"  document.addEventListener('click', function (e) {\n" +
		"    function onClick() {}\n" +
		"  })\n" +
		"})()\n" +
		"\n" +
		"$(function () {\n" +
		"  function ready() {}\n" +
		"})\n"
	checkSyms(t, "javascript", iife, []string{"func init 3-5", "const state 6-6", "func onClick 8-8", "func ready 13-13"})

	spec := "import { test, expect } from '@playwright/test'\n" +
		"\n" +
		"describe('merge readiness', () => {\n" +
		"  const helper = () => 1\n" +
		"  it('is ready when checks pass', async () => {\n" +
		"    expect(1).toBe(1)\n" +
		"  })\n" +
		"  describe.skip('nested', () => {\n" +
		"    test(\"deep one\", () => {})\n" +
		"  })\n" +
		"})\n" +
		"test('top level', async ({ page }) => {\n" +
		"  await page.goto('/')\n" +
		"})\n"
	checkSyms(t, "typescript", spec, []string{
		"suite merge readiness 3-11", "func merge readiness.helper 4-4", "test merge readiness.is ready when checks pass 5-7",
		"suite merge readiness.nested 8-10", "test nested.deep one 9-9", "test top level 12-14",
	}, "@playwright/test")
}

// Scanners must survive anything.
func TestScanNeverPanics(t *testing.T) {
	inputs := []string{"", "\x00\x01\xff", "{{{{", "}}}}", "`", "\"", "'", "/*", "r#\"", "fn", "class", "'a", "\n\n\n", strings.Repeat("(", 1000), strings.Repeat("{\n", 500)}
	for _, lang := range []string{"go", "python", "typescript", "tsx", "javascript", "rust", "java", "kotlin", "shell", "sql", "markdown", "yaml", "html"} {
		for _, in := range inputs {
			Scan(lang, []byte(in))
		}
	}
}
