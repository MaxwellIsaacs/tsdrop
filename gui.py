"""tsdrop window — styled from ~/.config/palette/palette.toml, like debrief."""
import json
import os
import subprocess
import sys
import threading
import tomllib
from pathlib import Path

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Gdk", "4.0")
from gi.repository import Gdk, Gio, GLib, Gtk

BIN = os.environ.get("TSDROP_BIN", "tsdrop")
FONT = "Geist Mono"

C = {"bg": "#1f2124", "bg_alt": "#25282c", "base1": "#2b2e33", "base2": "#33373c", "base3": "#3e4349",
     "base4": "#585e66", "fg": "#d8d4cc", "accent": "#88a4c0", "label": "#7d8590", "ok": "#a3b38c", "err": "#d0786a"}
try:
    PAL = tomllib.loads((Path.home() / ".config/palette/palette.toml").read_text())
    C.update(PAL["colors"])
    C.update({k: PAL["colors"][v] for k, v in PAL.get("roles", {}).items()})
except (OSError, KeyError, tomllib.TOMLDecodeError):
    pass

CSS = f"""
* {{ font-family: "{FONT}"; font-size: 10pt; border-radius: 0; box-shadow: none; text-shadow: none; outline: none; }}
window, .root {{ background: {C['bg']}; color: {C['fg']}; }}
.label {{ color: {C['label']}; font-weight: bold; font-size: 8.5pt; letter-spacing: 1px; }}
.hint {{ color: {C['base3']}; font-size: 8.5pt; }}
.keys {{ color: {C['base4']}; font-size: 8.5pt; }}
.dim {{ color: {C['label']}; }}
.faint {{ color: {C['base4']}; }}

list {{ background: transparent; }}
list row {{ background: transparent; padding: 7px 10px; border-left: 2px solid transparent; }}
list row:hover {{ background: {C['bg_alt']}; }}
list row:selected {{ background: {C['base1']}; border-left-color: {C['accent']}; color: {C['fg']}; }}
.dot-on {{ color: {C['ok']}; }}
.dot-off {{ color: {C['base3']}; }}

.drop {{ border: 1px dashed {C['base3']}; padding: 16px; background: transparent; }}
.drop.hover {{ border: 1px solid {C['accent']}; background: {C['bg_alt']}; }}
.drop.full {{ border-style: solid; border-color: {C['base2']}; }}

button {{ background: {C['base1']}; color: {C['fg']}; border: 1px solid {C['base2']}; padding: 4px 12px; min-height: 0; }}
button:hover {{ background: {C['base2']}; }}
button.flat {{ background: transparent; border-color: transparent; color: {C['label']}; padding: 0 4px; }}
button.flat:hover {{ color: {C['fg']}; }}
button.send {{ background: {C['accent']}; color: {C['bg']}; border-color: {C['accent']}; font-weight: bold; padding: 7px; }}
button.send:hover {{ background: {C['fg']}; border-color: {C['fg']}; }}
button.send:disabled {{ background: {C['base1']}; color: {C['base4']}; border-color: {C['base2']}; }}

entry {{ background: {C['base0'] if 'base0' in C else C['bg']}; color: {C['fg']}; border: 1px solid {C['base2']}; padding: 6px 8px; min-height: 0; caret-color: {C['accent']}; }}
entry:focus-within {{ border-color: {C['accent']}; }}
entry > text > placeholder {{ color: {C['base4']}; }}

progressbar trough {{ background: {C['base1']}; min-height: 2px; border: none; }}
progressbar progress {{ background: {C['accent']}; min-height: 2px; border: none; }}
.status.ok {{ color: {C['ok']}; }}
.status.err {{ color: {C['err']}; }}
scrollbar {{ background: transparent; }}
scrollbar slider {{ background: {C['base2']}; min-width: 3px; }}
"""


def label(text, *classes, **kw):
    kw.setdefault("xalign", 0)
    w = Gtk.Label(label=text, **kw)
    for c in classes:
        w.add_css_class(c)
    return w


def section(title, hint=""):
    head = Gtk.Box()
    head.append(label(title.upper(), "label", hexpand=True))
    if hint:
        head.append(label(hint, "hint"))
    return head


def ok(p):
    """Sendable: LocalSend answers, or an iPhone that pulls from the outbox."""
    return p["localsend"] or p.get("web", False)


class Win(Gtk.ApplicationWindow):
    def __init__(self, app, files):
        super().__init__(application=app, title="tsdrop")
        self.set_default_size(420, 560)
        self.set_decorated(False)
        self.files = list(files)
        self.peers = []
        self.raw_peers = None
        self.scanning = self.sending = False

        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
        box.add_css_class("root")
        for side in ("top", "bottom", "start", "end"):
            getattr(box, f"set_margin_{side}")(16)
        self.set_child(box)

        top = Gtk.Box()
        top.append(label("tsdrop", hexpand=True))
        self.count = label("", "faint")
        top.append(self.count)
        box.append(top)

        box.append(section("send to", "tailnet"))
        self.list = Gtk.ListBox(selection_mode=Gtk.SelectionMode.SINGLE)
        self.list.connect("row-selected", lambda *_: self.update_send())
        self.list.connect("row-activated", lambda *_: self.send())
        scroller = Gtk.ScrolledWindow(vexpand=True, child=self.list, hscrollbar_policy=Gtk.PolicyType.NEVER)
        scroller.set_min_content_height(140)
        box.append(scroller)

        box.append(section("files", "drag & drop"))
        self.drop = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
        self.drop.add_css_class("drop")
        self.drop_label = label("", "dim", xalign=0.5, wrap=True, justify=Gtk.Justification.CENTER)
        btns = Gtk.Box(spacing=6, halign=Gtk.Align.CENTER)
        choose = Gtk.Button(label="choose…")
        choose.connect("clicked", self.choose)
        self.clear_btn = Gtk.Button(label="clear")
        self.clear_btn.add_css_class("flat")
        self.clear_btn.connect("clicked", lambda *_: (self.files.clear(), self.update_files()))
        btns.append(choose)
        btns.append(self.clear_btn)
        self.drop.append(self.drop_label)
        self.drop.append(btns)
        target = Gtk.DropTarget.new(Gdk.FileList, Gdk.DragAction.COPY)
        target.connect("drop", self.on_drop)
        target.connect("enter", lambda *_: (self.drop.add_css_class("hover"), Gdk.DragAction.COPY)[1])
        target.connect("leave", lambda *_: self.drop.remove_css_class("hover"))
        self.drop.add_controller(target)
        box.append(self.drop)

        box.append(section("message", "text or link"))
        self.text = Gtk.Entry(placeholder_text="…")
        self.text.connect("changed", lambda *_: self.update_send())
        self.text.connect("activate", lambda *_: self.send())
        box.append(self.text)

        self.bar = Gtk.ProgressBar(visible=False, margin_top=6)
        box.append(self.bar)
        self.status = label("", "status", "dim", wrap=True)
        box.append(self.status)

        self.send_btn = Gtk.Button(label="send")
        self.send_btn.add_css_class("send")
        self.send_btn.connect("clicked", lambda *_: self.send())
        box.append(self.send_btn)
        box.append(label("enter send · j/k peer · o files · r rescan · esc close", "keys", xalign=0.5, margin_top=2))

        keys = Gtk.EventControllerKey()
        keys.connect("key-pressed", self.on_key)
        self.add_controller(keys)

        self.update_files()
        self.load_peers()
        GLib.timeout_add_seconds(3, self.poll)

    def on_key(self, _ctl, keyval, _code, state):
        if keyval == Gdk.KEY_Escape:
            self.close()
            return True
        if self.text.has_focus() or state & (Gdk.ModifierType.CONTROL_MASK | Gdk.ModifierType.ALT_MASK):
            return False
        name = Gdk.keyval_name(keyval)
        if name in ("j", "k"):
            self.move(1 if name == "j" else -1)
        elif name == "r":
            self.load_peers()
        elif name == "o":
            self.choose()
        elif name in ("slash", "m"):
            self.text.grab_focus()
        elif name in ("Return", "KP_Enter"):
            self.send()
        else:
            return False
        return True

    def move(self, step):
        ready = [i for i, p in enumerate(self.peers) if ok(p)]
        if not ready:
            return
        row = self.list.get_selected_row()
        cur = ready.index(row.get_index()) if row and row.get_index() in ready else -1
        self.list.select_row(self.list.get_row_at_index(ready[(cur + step) % len(ready)]))

    def set_status(self, text, kind=""):
        self.status.set_text(text)
        for k in ("ok", "err"):
            (self.status.add_css_class if k == kind else self.status.remove_css_class)(k)

    # peers ---------------------------------------------------------------
    def poll(self):
        # iOS suspends LocalSend whenever it leaves the foreground, so keep rescanning.
        if not self.scanning and not self.sending:
            self.load_peers(quiet=True)
        return GLib.SOURCE_CONTINUE

    def load_peers(self, quiet=False):
        if self.scanning:
            return
        self.scanning = True
        if not quiet:
            self.set_status("scanning tailnet…")

        def work():
            try:
                out = subprocess.run([BIN, "peers", "--json", "--timeout", "1500ms"], capture_output=True, text=True, check=True).stdout
                peers = json.loads(out)
            except Exception as e:  # noqa: BLE001 - surface anything in the UI
                GLib.idle_add(self.scan_failed, f"tailscale: {e}")
                return
            GLib.idle_add(self.show_peers, peers, quiet)

        threading.Thread(target=work, daemon=True).start()

    def scan_failed(self, msg):
        self.scanning = False
        self.set_status(msg, "err")

    def show_peers(self, peers, quiet=False):
        self.scanning = False
        if quiet and peers == self.raw_peers:
            return
        self.raw_peers = peers
        prev = self.selected()
        self.peers = sorted(peers, key=lambda p: (not ok(p), p["name"]))
        while (row := self.list.get_row_at_index(0)) is not None:
            self.list.remove(row)
        for p in self.peers:
            row = Gtk.ListBoxRow(selectable=ok(p), activatable=ok(p))
            line = Gtk.Box(spacing=10)
            line.append(label("●" if ok(p) else "○", "dot-on" if ok(p) else "dot-off"))
            texts = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, hexpand=True)
            texts.append(label(p.get("alias") or p["name"], "" if ok(p) else "dim"))
            sub = f'{p["name"]} · ' if p.get("alias") else ""
            texts.append(label(sub + f'{p["os"].lower()} · {p["ip"]}', "faint"))
            line.append(texts)
            if p.get("web"):
                line.append(label("via outbox", "faint"))
            elif not ok(p):
                line.append(label("not open", "faint"))
            row.set_child(line)
            self.list.append(row)
            if prev and p["ip"] == prev["ip"] and ok(p):
                self.list.select_row(row)
        ready = [p for p in self.peers if ok(p)]
        if not self.selected() and ready:
            self.list.select_row(self.list.get_row_at_index(0))
        self.count.set_text(f"{len(ready)}/{len(self.peers)} ready")
        if not self.sending and not (quiet and self.status.has_css_class("ok")):
            self.set_status("" if ready else "open LocalSend on the other device")
        self.update_send()

    def selected(self):
        row = self.list.get_selected_row()
        return self.peers[row.get_index()] if row is not None and row.get_index() < len(self.peers) else None

    # files ---------------------------------------------------------------
    def on_drop(self, _target, value, *_):
        self.drop.remove_css_class("hover")
        self.files += [f.get_path() for f in value.get_files() if f.get_path()]
        self.update_files()
        return True

    def choose(self, *_):
        dialog = Gtk.FileDialog(title="tsdrop — choose files")

        def done(d, res):
            try:
                picked = d.open_multiple_finish(res)
            except GLib.Error:
                return
            self.files += [picked.get_item(i).get_path() for i in range(picked.get_n_items())]
            self.update_files()

        dialog.open_multiple(self, None, done)

    def update_files(self):
        names = [os.path.basename(f.rstrip("/")) for f in self.files]
        if not names:
            self.drop_label.set_text("drop files here")
        elif len(names) <= 3:
            self.drop_label.set_text("\n".join(names))
        else:
            self.drop_label.set_text("\n".join(names[:2]) + f"\n+ {len(names) - 2} more")
        (self.drop.add_css_class if names else self.drop.remove_css_class)("full")
        self.drop_label.set_css_classes([] if names else ["dim"])
        self.clear_btn.set_visible(bool(names))
        self.update_send()

    # send ----------------------------------------------------------------
    def update_send(self):
        self.send_btn.set_sensitive(bool(self.selected() and (self.files or self.text.get_text().strip())))

    def send(self):
        peer = self.selected()
        text = self.text.get_text().strip()
        if not peer or not (self.files or text):
            return
        cmd = [BIN, "send", "--json", "--to", peer["ip"]]
        if text:
            cmd += ["--text", text]
        cmd += ["--"] + self.files
        self.sending = True
        self.send_btn.set_sensitive(False)
        self.bar.set_visible(True)
        self.bar.set_fraction(0)
        self.set_status(f"waiting for {peer.get('alias') or peer['name']} to accept…")
        threading.Thread(target=self.run_send, args=(cmd,), daemon=True).start()

    def run_send(self, cmd):
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        for line in proc.stdout:
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            if ev["event"] == "progress" and ev["total"]:
                GLib.idle_add(self.on_progress, ev)
        err = proc.stderr.read().strip()
        GLib.idle_add(self.finished, proc.wait(), err)

    def on_progress(self, ev):
        self.bar.set_fraction(ev["sent"] / ev["total"])
        self.set_status(f'sending {ev["file"]}  {ev["sent"] * 100 // ev["total"]}%')

    def finished(self, code, err):
        self.sending = False
        self.bar.set_visible(False)
        if code == 0:
            self.set_status("✓ sent", "ok")
            self.files.clear()
            self.text.set_text("")
            self.update_files()
        else:
            self.set_status(err.removeprefix("tsdrop: ") or "send failed", "err")
        self.update_send()


class App(Gtk.Application):
    def __init__(self):
        super().__init__(application_id="dev.tsdrop.gui", flags=Gio.ApplicationFlags.NON_UNIQUE)

    def do_activate(self):
        Gtk.Window.set_default_icon_name("tsdrop")
        Gtk.Settings.get_default().set_property("gtk-interface-color-scheme", 2)  # dark
        provider = Gtk.CssProvider()
        provider.load_from_string(CSS)
        Gtk.StyleContext.add_provider_for_display(Gdk.Display.get_default(), provider, Gtk.STYLE_PROVIDER_PRIORITY_USER + 1)
        Win(self, [os.path.abspath(f) for f in sys.argv[1:]]).present()


App().run([sys.argv[0]])
