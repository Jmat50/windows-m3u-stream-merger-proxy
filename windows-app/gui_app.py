#!/usr/bin/env python3
from __future__ import annotations

import json
import hashlib
import msvcrt
import os
import queue
import re
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import traceback
import urllib.parse
import urllib.request
import webbrowser
from collections import deque
from pathlib import Path
from typing import Callable
import tkinter as tk
from tkinter import filedialog, messagebox, ttk

import pystray
from PIL import Image, ImageDraw, ImageTk

try:
    import winreg
except ImportError:
    winreg = None

APP_NAME = "Windows M3U Stream Merger Proxy Desktop"
APP_STATE_DIR = Path(os.getenv("LOCALAPPDATA", str(Path.home()))) / "WindowsM3UStreamMergerProxyDesktop"
SETTINGS_FILE = APP_STATE_DIR / "settings.json"
APP_LOG_FILE = APP_STATE_DIR / "app.log"
APP_ERROR_LOG_FILE = APP_STATE_DIR / "error-detail.log"
RUNTIME_DIR = APP_STATE_DIR / "runtime"
DATA_DIR = APP_STATE_DIR / "data"
TEMP_DIR = APP_STATE_DIR / "temp"
RESTORE_SIGNAL_FILE = APP_STATE_DIR / "restore.signal"
WINDOWS_RUN_KEY = r"Software\Microsoft\Windows\CurrentVersion\Run"
WINDOWS_RUN_VALUE = "WindowsM3UStreamMergerProxyDesktop"

DEFAULT_SETTINGS = {
    "port": "8080",
    "base_url": "",
    "timezone": "America/New_York",
    "sync_cron": "0 0 * * *",
    "sync_on_boot": True,
    "clear_on_boot": False,
    "credentials": "",
    "max_retries": "5",
    "retry_wait": "0",
    "stream_timeout": "7",
    "start_on_boot": False,
    "sources": [],
    "include_title_filters": [],
    "exclude_title_filters": [],
    "channel_source_rules": [],
    "channel_merge_rules": [],
    "web_discovery_jobs": [],
}

ANSI_ESCAPE_RE = re.compile(r"\x1B\[[0-?]*[ -/]*[@-~]")


def _resource_root() -> Path:
    if getattr(sys, "frozen", False):
        return Path(getattr(sys, "_MEIPASS", Path(sys.executable).resolve().parent))
    return Path(__file__).resolve().parent


def _to_bool_env(value: bool) -> str:
    return "true" if value else "false"


def _is_port_available(port: int) -> bool:
    # Fast path: if something is actively listening, connect succeeds.
    for host in ("127.0.0.1", "localhost"):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.settimeout(0.2)
            try:
                if probe.connect_ex((host, port)) == 0:
                    return False
            except OSError:
                pass

    # Bind checks without SO_REUSEADDR to avoid false positives on Windows.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock_v4:
        try:
            sock_v4.bind(("0.0.0.0", port))
        except OSError:
            return False

    if socket.has_ipv6:
        try:
            with socket.socket(socket.AF_INET6, socket.SOCK_STREAM) as sock_v6:
                try:
                    sock_v6.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
                except OSError:
                    pass
                sock_v6.bind(("::", port, 0, 0))
        except OSError:
            return False

    return True


def _detect_lan_ipv4() -> str:
    # Preferred: infer active outbound interface IP.
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
            probe.connect(("8.8.8.8", 80))
            ip = probe.getsockname()[0].strip()
            if ip and not ip.startswith("127."):
                return ip
    except OSError:
        pass

    # Fallback: resolve local hostname.
    try:
        host = socket.gethostname()
        _, _, addrs = socket.gethostbyname_ex(host)
        for ip in addrs:
            ip = str(ip).strip()
            if ip and not ip.startswith("127."):
                return ip
    except OSError:
        pass

    return "127.0.0.1"


def _is_local_only_host(hostname: str) -> bool:
    host = str(hostname or "").strip().lower()
    return host in {"localhost", "127.0.0.1", "::1", "0.0.0.0"}


def _strip_ansi(text: str) -> str:
    return ANSI_ESCAPE_RE.sub("", text)


def _get_listening_processes(port: int) -> list[dict[str, str]]:
    if os.name != "nt":
        return []

    script = f"""
$ErrorActionPreference = 'SilentlyContinue'
$conns = Get-NetTCPConnection -State Listen -LocalPort {port}
$out = @()
foreach ($c in $conns) {{
  $procId = [int]$c.OwningProcess
  $proc = Get-CimInstance Win32_Process -Filter "ProcessId=$procId"
  $out += [PSCustomObject]@{{
    Pid = "$procId"
    Name = "$($proc.Name)"
    Path = "$($proc.ExecutablePath)"
  }}
}}
$out | ConvertTo-Json -Compress
"""
    try:
        result = subprocess.run(
            ["powershell", "-NoProfile", "-Command", script],
            capture_output=True,
            text=True,
            check=True,
            timeout=4,
        )
    except Exception:  # noqa: BLE001
        return []

    raw = result.stdout.strip()
    if not raw:
        return []

    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return []

    if isinstance(data, dict):
        return [data]
    if isinstance(data, list):
        return [d for d in data if isinstance(d, dict)]
    return []


def _get_processes_by_name(names: list[str]) -> list[dict[str, str]]:
    if os.name != "nt" or not names:
        return []

    quoted = ",".join([f'"{name}"' for name in names])
    script = f"""
$ErrorActionPreference = 'SilentlyContinue'
$wanted = @({quoted})
$out = @()
$procs = Get-CimInstance Win32_Process | Where-Object {{ $wanted -contains $_.Name }}
foreach ($proc in $procs) {{
  $out += [PSCustomObject]@{{
    Pid = "$($proc.ProcessId)"
    Name = "$($proc.Name)"
    Path = "$($proc.ExecutablePath)"
  }}
}}
$out | ConvertTo-Json -Compress
"""
    try:
        result = subprocess.run(
            ["powershell", "-NoProfile", "-Command", script],
            capture_output=True,
            text=True,
            check=True,
            timeout=6,
        )
    except Exception:  # noqa: BLE001
        return []

    raw = result.stdout.strip()
    if not raw:
        return []

    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return []

    if isinstance(data, dict):
        return [data]
    if isinstance(data, list):
        return [d for d in data if isinstance(d, dict)]
    return []


def _terminate_pid(pid: int) -> bool:
    if os.name == "nt":
        try:
            proc = subprocess.run(
                ["taskkill", "/PID", str(pid), "/T", "/F"],
                capture_output=True,
                text=True,
                timeout=4,
            )
            return proc.returncode == 0
        except Exception:  # noqa: BLE001
            return False
    try:
        os.kill(pid, signal.SIGTERM)
        return True
    except Exception:  # noqa: BLE001
        return False


class DesktopApp(tk.Tk):
    def __init__(self) -> None:
        super().__init__()
        self.title(APP_NAME)
        self.geometry("1100x780")
        self.minsize(980, 680)

        self.server_process: subprocess.Popen[str] | None = None
        self.log_queue: queue.Queue[str] = queue.Queue()
        self.tray_icon: pystray.Icon | None = None
        self.is_quitting = False
        self.startup_blocked = False
        self.instance_lock_fd = None

        self.port_var = tk.StringVar()
        self.base_url_var = tk.StringVar()
        self.timezone_var = tk.StringVar()
        self.sync_cron_var = tk.StringVar()
        self.credentials_var = tk.StringVar()
        self.max_retries_var = tk.StringVar()
        self.retry_wait_var = tk.StringVar()
        self.stream_timeout_var = tk.StringVar()
        self.source_name_var = tk.StringVar()
        self.source_url_var = tk.StringVar()
        self.start_on_boot_var = tk.BooleanVar()
        self.sync_on_boot_var = tk.BooleanVar()
        self.clear_on_boot_var = tk.BooleanVar()
        self.status_var = tk.StringVar(value="STOPPED")
        self.status_detail_var = tk.StringVar(value="Server process is not running.")
        self.playlist_url_var = tk.StringVar(value="Playlist URL: not available")
        self.last_event_var = tk.StringVar(value="Last event: app started")
        self.last_exit_code: int | None = None
        self.last_server_error: str | None = None
        self.last_server_line: str | None = None
        self.stop_requested_by_user = False
        self.recent_log_lines: deque[str] = deque(maxlen=250)
        self.error_log_window: tk.Toplevel | None = None
        self.error_log_text_widget: tk.Text | None = None
        self.include_title_filters: list[str] = []
        self.exclude_title_filters: list[str] = []
        self.channel_source_rules: list[dict[str, object]] = []
        self.channel_merge_rules: list[dict[str, str]] = []
        self.web_discovery_jobs: list[dict[str, object]] = []
        self.sources_items: list[dict[str, str]] = []
        self.source_action_icons: dict[str, tk.PhotoImage] = {}
        self.channel_source_reorder_callback: Callable[[], None] | None = None

        self.sources_tree: ttk.Treeview
        self.log_text: tk.Text
        self.status_state_entry: tk.Entry

        self._bootstrap_dirs()
        if not self._acquire_instance_lock():
            self._signal_existing_instance_restore()
            self.startup_blocked = True
            self.destroy()
            return

        self._build_ui()
        self._install_exception_hooks()
        self._load_settings_into_ui()
        self._update_status_display()

        self.after(150, self._drain_log_queue)
        self.after(500, self._refresh_process_state)
        self.after(600, self._autostart_server)
        self.after(700, self._poll_restore_requests)
        self.protocol("WM_DELETE_WINDOW", self._on_close)

    def _bootstrap_dirs(self) -> None:
        APP_STATE_DIR.mkdir(parents=True, exist_ok=True)
        RUNTIME_DIR.mkdir(parents=True, exist_ok=True)
        DATA_DIR.mkdir(parents=True, exist_ok=True)
        TEMP_DIR.mkdir(parents=True, exist_ok=True)
        try:
            if RESTORE_SIGNAL_FILE.exists():
                RESTORE_SIGNAL_FILE.unlink()
        except OSError:
            pass

    def _install_exception_hooks(self) -> None:
        def handle_sys_exception(exc_type: type[BaseException], exc_value: BaseException, exc_tb: object) -> None:
            message = "".join(traceback.format_exception(exc_type, exc_value, exc_tb))
            self._record_app_error(f"Unhandled exception:\n{message}")

        def handle_thread_exception(args: threading.ExceptHookArgs) -> None:
            message = "".join(traceback.format_exception(args.exc_type, args.exc_value, args.exc_traceback))
            self._record_app_error(
                f"Unhandled thread exception in '{args.thread.name if args.thread else 'unknown'}':\n{message}"
            )

        sys.excepthook = handle_sys_exception
        if hasattr(threading, "excepthook"):
            threading.excepthook = handle_thread_exception

    def report_callback_exception(self, exc: type[BaseException], val: BaseException, tb: object) -> None:
        message = "".join(traceback.format_exception(exc, val, tb))
        self._record_app_error(f"Tk callback exception:\n{message}")
        messagebox.showerror("Application Error", str(val))

    def _signal_existing_instance_restore(self) -> None:
        try:
            APP_STATE_DIR.mkdir(parents=True, exist_ok=True)
            RESTORE_SIGNAL_FILE.write_text(str(time.time()), encoding="utf-8")
        except OSError:
            # Best-effort signal; ignore failures silently.
            pass

    def _poll_restore_requests(self) -> None:
        if self.is_quitting:
            return

        should_restore = False
        try:
            if RESTORE_SIGNAL_FILE.exists():
                RESTORE_SIGNAL_FILE.unlink()
                should_restore = True
        except OSError:
            should_restore = False

        if should_restore:
            self._restore_from_tray()
            self._set_last_event("Another app launch requested focus.")

        self.after(700, self._poll_restore_requests)

    def _acquire_instance_lock(self) -> bool:
        lock_path = APP_STATE_DIR / "app.lock"
        lock_file = None
        try:
            lock_file = open(lock_path, "a+b")
            lock_file.seek(0)
            lock_file.write(b"0")
            lock_file.flush()
            lock_file.seek(0)
            msvcrt.locking(lock_file.fileno(), msvcrt.LK_NBLCK, 1)
            self.instance_lock_fd = lock_file
            return True
        except OSError:
            try:
                lock_file.close()
            except Exception:  # noqa: BLE001
                pass
            return False

    def _release_instance_lock(self) -> None:
        if self.instance_lock_fd is None:
            return
        try:
            self.instance_lock_fd.seek(0)
            msvcrt.locking(self.instance_lock_fd.fileno(), msvcrt.LK_UNLCK, 1)
        except OSError:
            pass
        try:
            self.instance_lock_fd.close()
        except OSError:
            pass
        self.instance_lock_fd = None

    def _build_ui(self) -> None:
        root = ttk.Frame(self, padding=12)
        root.pack(fill=tk.BOTH, expand=True)

        for col in range(4):
            root.columnconfigure(col, weight=1)
        root.rowconfigure(6, weight=3)
        root.rowconfigure(9, weight=0)
        root.rowconfigure(11, weight=2)

        ttk.Label(root, text="Port").grid(row=0, column=0, sticky="w")
        ttk.Entry(root, textvariable=self.port_var).grid(row=0, column=1, sticky="ew", padx=(0, 10))
        ttk.Label(root, text="Base URL").grid(row=0, column=2, sticky="w")
        ttk.Entry(root, textvariable=self.base_url_var).grid(row=0, column=3, sticky="ew")

        ttk.Label(root, text="Timezone").grid(row=1, column=0, sticky="w", pady=(8, 0))
        ttk.Entry(root, textvariable=self.timezone_var).grid(
            row=1, column=1, sticky="ew", padx=(0, 10), pady=(8, 0)
        )
        ttk.Label(root, text="Sync Cron").grid(row=1, column=2, sticky="w", pady=(8, 0))
        ttk.Entry(root, textvariable=self.sync_cron_var).grid(row=1, column=3, sticky="ew", pady=(8, 0))

        ttk.Label(root, text="Credentials").grid(row=2, column=0, sticky="w", pady=(8, 0))
        ttk.Entry(root, textvariable=self.credentials_var).grid(
            row=2, column=1, sticky="ew", padx=(0, 10), pady=(8, 0)
        )
        ttk.Label(root, text="Max Retries").grid(row=2, column=2, sticky="w", pady=(8, 0))
        ttk.Entry(root, textvariable=self.max_retries_var).grid(row=2, column=3, sticky="ew", pady=(8, 0))

        ttk.Label(root, text="Retry Wait (s)").grid(row=3, column=0, sticky="w", pady=(8, 0))
        ttk.Entry(root, textvariable=self.retry_wait_var).grid(
            row=3, column=1, sticky="ew", padx=(0, 10), pady=(8, 0)
        )
        ttk.Label(root, text="Stream Timeout (s)").grid(row=3, column=2, sticky="w", pady=(8, 0))
        ttk.Entry(root, textvariable=self.stream_timeout_var).grid(row=3, column=3, sticky="ew", pady=(8, 0))

        ttk.Checkbutton(root, text="Sync On Boot", variable=self.sync_on_boot_var).grid(
            row=4, column=0, sticky="w", pady=(8, 0)
        )
        ttk.Checkbutton(root, text="Clear On Boot", variable=self.clear_on_boot_var).grid(
            row=4, column=1, sticky="w", pady=(8, 0)
        )
        ttk.Checkbutton(root, text="Start on Boot", variable=self.start_on_boot_var).grid(
            row=4, column=2, sticky="w", pady=(8, 0)
        )

        ttk.Label(root, text="M3U Sources (order defines source index)").grid(
            row=5, column=0, columnspan=4, sticky="w", pady=(12, 4)
        )
        sources_frame = ttk.Frame(root)
        sources_frame.grid(row=6, column=0, columnspan=4, sticky="nsew")
        sources_frame.columnconfigure(0, weight=1, minsize=420)
        sources_frame.rowconfigure(0, weight=1)

        source_list_frame = ttk.Frame(sources_frame)
        source_list_frame.grid(row=0, column=0, sticky="nsew")
        source_list_frame.columnconfigure(0, weight=1)
        source_list_frame.columnconfigure(1, weight=0)
        source_list_frame.columnconfigure(2, weight=0)
        source_list_frame.rowconfigure(0, weight=1)

        self.sources_tree = ttk.Treeview(
            source_list_frame,
            columns=("index", "name", "url", "concurrency"),
            show="headings",
            selectmode="browse",
            height=10,
        )
        self.sources_tree.heading("index", text="#")
        self.sources_tree.heading("name", text="Name")
        self.sources_tree.heading("url", text="Source URL")
        self.sources_tree.heading("concurrency", text="Max")
        self.sources_tree.column("index", width=45, anchor=tk.CENTER, stretch=False)
        self.sources_tree.column("name", width=140, anchor=tk.W, stretch=False)
        self.sources_tree.column("url", width=360, anchor=tk.W, stretch=True)
        self.sources_tree.column("concurrency", width=55, anchor=tk.CENTER, stretch=False)
        self.sources_tree.grid(row=0, column=0, sticky="nsew")
        self.sources_tree.bind("<<TreeviewSelect>>", self._on_source_tree_select)
        sources_scroll_y = ttk.Scrollbar(source_list_frame, orient=tk.VERTICAL, command=self.sources_tree.yview)
        sources_scroll_y.grid(row=0, column=1, sticky="ns")
        self.sources_tree.configure(yscrollcommand=sources_scroll_y.set)

        if not self.source_action_icons:
            self.source_action_icons = {
                "add": self._create_source_toolbar_icon("add"),
                "remove": self._create_source_toolbar_icon("remove"),
                "up": self._create_source_toolbar_icon("up"),
                "down": self._create_source_toolbar_icon("down"),
            }

        toolbar_style = ttk.Style(self)
        toolbar_style.configure("SourceIcon.TButton", padding=1)

        source_actions = ttk.Frame(source_list_frame)
        source_actions.grid(row=0, column=2, sticky="ne", padx=(6, 0), pady=(2, 0))
        ttk.Button(
            source_actions,
            image=self.source_action_icons["add"],
            command=self._open_add_source_dialog,
            style="SourceIcon.TButton",
            width=2,
        ).grid(row=0, column=0, pady=(0, 4))
        ttk.Button(
            source_actions,
            image=self.source_action_icons["remove"],
            command=self._remove_source_item_clicked,
            style="SourceIcon.TButton",
            width=2,
        ).grid(row=1, column=0, pady=(0, 4))
        ttk.Button(
            source_actions,
            image=self.source_action_icons["up"],
            command=self._move_source_up_clicked,
            style="SourceIcon.TButton",
            width=2,
        ).grid(row=2, column=0, pady=(0, 4))
        ttk.Button(
            source_actions,
            image=self.source_action_icons["down"],
            command=self._move_source_down_clicked,
            style="SourceIcon.TButton",
            width=2,
        ).grid(row=3, column=0)

        controls = ttk.Frame(root)
        controls.grid(row=7, column=0, columnspan=4, sticky="ew", pady=(10, 0))
        controls.columnconfigure(9, weight=1)

        ttk.Button(controls, text="Save Settings", command=self._save_settings_clicked).grid(row=0, column=0, padx=(0, 8))
        ttk.Button(controls, text="Reset", width=7, command=self._reset_settings_to_defaults_clicked).grid(
            row=0, column=1, padx=(0, 8)
        )
        ttk.Button(controls, text="Channel Settings", command=self._open_channel_settings_popup).grid(
            row=0, column=2, padx=(0, 8)
        )
        ttk.Button(controls, text="Web Discovery", command=self._open_web_discovery_popup).grid(
            row=0, column=3, padx=(0, 8)
        )
        ttk.Button(controls, text="Export Backup", command=self._export_backup_clicked).grid(
            row=0, column=4, padx=(0, 8)
        )
        ttk.Button(controls, text="Load Backup", command=self._load_backup_clicked).grid(row=0, column=5, padx=(0, 8))
        ttk.Button(controls, text="Open Playlist", command=self._open_playlist_clicked).grid(row=0, column=6, padx=(0, 8))
        ttk.Button(controls, text="Open Data Folder", command=self._open_data_folder_clicked).grid(row=0, column=7, padx=(0, 8))
        ttk.Button(controls, text="Error Log", command=self._open_error_log_clicked).grid(row=0, column=8)

        status_frame = ttk.LabelFrame(root, text="Server Status", padding=8)
        status_frame.grid(row=8, column=0, columnspan=4, sticky="ew", pady=(10, 4))
        status_frame.columnconfigure(1, weight=1)

        ttk.Label(status_frame, text="State:", width=10).grid(row=0, column=0, sticky="w")
        self.status_state_entry = tk.Entry(
            status_frame,
            textvariable=self.status_var,
            state="readonly",
            relief=tk.FLAT,
            readonlybackground="white",
            font=("Segoe UI", 11, "bold"),
        )
        self.status_state_entry.grid(row=0, column=1, sticky="ew")

        ttk.Label(status_frame, text="Details:", width=10).grid(row=1, column=0, sticky="w", pady=(4, 0))
        tk.Entry(
            status_frame,
            textvariable=self.status_detail_var,
            state="readonly",
            relief=tk.FLAT,
            readonlybackground="white",
        ).grid(row=1, column=1, sticky="ew", pady=(4, 0))

        ttk.Label(status_frame, text="Playlist:", width=10).grid(row=2, column=0, sticky="w", pady=(4, 0))
        tk.Entry(
            status_frame,
            textvariable=self.playlist_url_var,
            state="readonly",
            relief=tk.FLAT,
            readonlybackground="white",
        ).grid(row=2, column=1, sticky="ew", pady=(4, 0))

        ttk.Label(status_frame, text="Last Event:", width=10).grid(row=3, column=0, sticky="w", pady=(4, 0))
        tk.Entry(
            status_frame,
            textvariable=self.last_event_var,
            state="readonly",
            relief=tk.FLAT,
            readonlybackground="white",
        ).grid(row=3, column=1, sticky="ew", pady=(4, 0))

        status_actions = ttk.Frame(status_frame)
        status_actions.grid(row=4, column=0, columnspan=2, sticky="e", pady=(10, 0))
        ttk.Button(status_actions, text="Start Server", command=self._start_server_clicked).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(status_actions, text="Stop Server", command=self._stop_server_clicked).pack(side=tk.LEFT)

        ttk.Label(root, text="Logs").grid(row=10, column=0, columnspan=4, sticky="w", pady=(8, 4))
        logs_frame = ttk.Frame(root)
        logs_frame.grid(row=11, column=0, columnspan=4, sticky="nsew")
        logs_frame.columnconfigure(0, weight=1)
        logs_frame.rowconfigure(0, weight=1)

        self.log_text = tk.Text(logs_frame, wrap=tk.WORD, state=tk.DISABLED)
        self.log_text.grid(row=0, column=0, sticky="nsew")
        logs_scroll_y = ttk.Scrollbar(logs_frame, orient=tk.VERTICAL, command=self.log_text.yview)
        logs_scroll_y.grid(row=0, column=1, sticky="ns")
        self.log_text.configure(yscrollcommand=logs_scroll_y.set)

    def _create_source_toolbar_icon(self, action: str) -> tk.PhotoImage:
        size = 18
        icon = Image.new("RGBA", (size, size), (0, 0, 0, 0))
        draw = ImageDraw.Draw(icon)
        draw.rounded_rectangle((1, 1, size - 2, size - 2), radius=4, fill=(241, 244, 247, 255), outline=(157, 166, 176, 255))

        fg = (35, 43, 52, 255)
        center = size // 2

        if action == "add":
            draw.line((center, 5, center, size - 5), fill=fg, width=2)
            draw.line((5, center, size - 5, center), fill=fg, width=2)
        elif action == "remove":
            draw.line((5, center, size - 5, center), fill=fg, width=2)
        elif action == "up":
            draw.polygon([(center, 4), (5, 10), (size - 5, 10)], fill=fg)
            draw.rectangle((center - 1, 10, center + 1, size - 4), fill=fg)
        elif action == "down":
            draw.rectangle((center - 1, 4, center + 1, size - 10), fill=fg)
            draw.polygon([(5, size - 10), (size - 5, size - 10), (center, size - 4)], fill=fg)
        else:
            draw.ellipse((6, 6, size - 6, size - 6), fill=fg)

        return ImageTk.PhotoImage(icon)

    def _create_channel_transfer_icon(self) -> tk.PhotoImage:
        size = 20
        icon = Image.new("RGBA", (size, size), (0, 0, 0, 0))
        draw = ImageDraw.Draw(icon)
        draw.rounded_rectangle((1, 1, size - 2, size - 2), radius=5, fill=(241, 244, 247, 255), outline=(157, 166, 176, 255))
        fg = (35, 43, 52, 255)
        mid = size // 2

        # Right-facing arrow.
        draw.polygon([(5, mid - 3), (10, mid - 3), (10, mid - 6), (15, mid), (10, mid + 6), (10, mid + 3), (5, mid + 3)], fill=fg)
        # Left-facing arrow.
        draw.polygon([(15, mid - 3), (10, mid - 3), (10, mid - 6), (5, mid), (10, mid + 6), (10, mid + 3), (15, mid + 3)], fill=fg)

        return ImageTk.PhotoImage(icon)

    def _load_settings(self) -> dict:
        if not SETTINGS_FILE.exists():
            return dict(DEFAULT_SETTINGS)

        try:
            loaded = json.loads(SETTINGS_FILE.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return dict(DEFAULT_SETTINGS)

        return self._merge_with_default_settings(loaded)

    def _merge_with_default_settings(self, loaded: object) -> dict:
        settings = dict(DEFAULT_SETTINGS)
        if not isinstance(loaded, dict):
            return settings

        for key in settings:
            if key in loaded:
                settings[key] = loaded[key]

        if not isinstance(settings.get("sources"), list):
            settings["sources"] = []
        if not isinstance(settings.get("include_title_filters"), list):
            settings["include_title_filters"] = []
        if not isinstance(settings.get("exclude_title_filters"), list):
            settings["exclude_title_filters"] = []
        if not isinstance(settings.get("channel_source_rules"), list):
            settings["channel_source_rules"] = []
        if not isinstance(settings.get("channel_merge_rules"), list):
            settings["channel_merge_rules"] = []
        if not isinstance(settings.get("web_discovery_jobs"), list):
            settings["web_discovery_jobs"] = []

        return settings

    def _load_settings_into_ui(self) -> None:
        settings = self._load_settings()
        self._apply_settings_to_ui(settings)
        self._sync_start_on_boot(
            bool(settings.get("start_on_boot", DEFAULT_SETTINGS["start_on_boot"])),
            show_dialogs=False,
            log_event=False,
        )

    def _apply_settings_to_ui(self, settings: dict) -> None:
        self.port_var.set(str(settings.get("port", DEFAULT_SETTINGS["port"])))
        self.base_url_var.set(str(settings.get("base_url", DEFAULT_SETTINGS["base_url"])))
        self.timezone_var.set(str(settings.get("timezone", DEFAULT_SETTINGS["timezone"])))
        self.sync_cron_var.set(str(settings.get("sync_cron", DEFAULT_SETTINGS["sync_cron"])))
        self.credentials_var.set(str(settings.get("credentials", DEFAULT_SETTINGS["credentials"])))
        self.max_retries_var.set(str(settings.get("max_retries", DEFAULT_SETTINGS["max_retries"])))
        self.retry_wait_var.set(str(settings.get("retry_wait", DEFAULT_SETTINGS["retry_wait"])))
        self.stream_timeout_var.set(str(settings.get("stream_timeout", DEFAULT_SETTINGS["stream_timeout"])))
        self.start_on_boot_var.set(bool(settings.get("start_on_boot", DEFAULT_SETTINGS["start_on_boot"])))
        self.sync_on_boot_var.set(bool(settings.get("sync_on_boot", DEFAULT_SETTINGS["sync_on_boot"])))
        self.clear_on_boot_var.set(bool(settings.get("clear_on_boot", DEFAULT_SETTINGS["clear_on_boot"])))

        self.sources_items = self._normalize_sources(settings.get("sources", []))
        self._refresh_sources_tree()
        self._clear_source_editor()
        self.include_title_filters = self._normalize_filter_list(settings.get("include_title_filters"))
        self.exclude_title_filters = self._normalize_filter_list(settings.get("exclude_title_filters"))
        self.channel_source_rules = self._normalize_channel_source_rules(settings.get("channel_source_rules"))
        self.channel_merge_rules = self._normalize_channel_merge_rules(settings.get("channel_merge_rules"))
        self.web_discovery_jobs = self._normalize_web_discovery_jobs(settings.get("web_discovery_jobs"))

    def _reset_settings_to_defaults_clicked(self) -> None:
        self._apply_settings_to_ui(DEFAULT_SETTINGS)
        self._append_log("[APP] Input fields reset to defaults. Click Save Settings to persist.")

    def _normalize_filter_list(self, value: object) -> list[str]:
        if not isinstance(value, list):
            return []

        output: list[str] = []
        for item in value:
            pattern = str(item).strip()
            if pattern:
                output.append(pattern)
        return output

    def _startup_supported(self) -> bool:
        return os.name == "nt" and winreg is not None

    def _build_startup_command(self) -> str:
        if getattr(sys, "frozen", False):
            return f'"{Path(sys.executable).resolve()}"'

        python_exec = Path(sys.executable).resolve()
        pythonw = python_exec.with_name("pythonw.exe")
        if pythonw.exists():
            python_exec = pythonw
        return f'"{python_exec}" "{Path(__file__).resolve()}"'

    def _is_start_on_boot_enabled(self) -> bool:
        if not self._startup_supported():
            return False
        try:
            with winreg.OpenKey(winreg.HKEY_CURRENT_USER, WINDOWS_RUN_KEY, 0, winreg.KEY_READ) as key:
                value, _ = winreg.QueryValueEx(key, WINDOWS_RUN_VALUE)
                return str(value).strip() != ""
        except FileNotFoundError:
            return False
        except OSError:
            return False

    def _set_start_on_boot_enabled(self, enabled: bool) -> None:
        if not self._startup_supported():
            return

        with winreg.CreateKey(winreg.HKEY_CURRENT_USER, WINDOWS_RUN_KEY) as key:
            if enabled:
                winreg.SetValueEx(key, WINDOWS_RUN_VALUE, 0, winreg.REG_SZ, self._build_startup_command())
            else:
                try:
                    winreg.DeleteValue(key, WINDOWS_RUN_VALUE)
                except FileNotFoundError:
                    pass

    def _sync_start_on_boot(self, enabled: bool, show_dialogs: bool, log_event: bool) -> None:
        if not self._startup_supported():
            return

        try:
            current = self._is_start_on_boot_enabled()
            if current != enabled:
                self._set_start_on_boot_enabled(enabled)

            if log_event:
                state_text = "enabled" if enabled else "disabled"
                self._append_log(f"[APP] Start on Boot {state_text}.")
        except OSError as exc:
            if show_dialogs:
                self._show_error("Start on Boot", f"Could not update Start on Boot setting:\n{exc}")
            self._append_log(f"[APP] Failed to update Start on Boot: {exc}")

    def _normalize_sources(self, value: object) -> list[dict[str, str]]:
        if not isinstance(value, list):
            return []

        output: list[dict[str, str]] = []
        for index, item in enumerate(value, start=1):
            if isinstance(item, dict):
                url = str(item.get("url", "")).strip()
                if not url:
                    continue
                name = str(item.get("name", "")).strip() or f"Source {index}"
                concurrency = str(item.get("concurrency", "1")).strip() or "1"
                if not concurrency.isdigit() or int(concurrency) < 1:
                    concurrency = "1"
                output.append(
                    {
                        "name": name,
                        "url": url,
                        "concurrency": concurrency,
                    }
                )
                continue

            if isinstance(item, str):
                url = item.strip()
                if url:
                    output.append({"name": f"Source {index}", "url": url, "concurrency": "1"})

        return output

    def _normalize_web_discovery_jobs(self, value: object) -> list[dict[str, object]]:
        if not isinstance(value, list):
            return []

        def _positive_int(raw: object, default: int) -> int:
            try:
                parsed = int(str(raw).strip())
            except (TypeError, ValueError):
                return default
            return parsed if parsed > 0 else default

        def _nonnegative_int(raw: object, default: int) -> int:
            try:
                parsed = int(str(raw).strip())
            except (TypeError, ValueError):
                return default
            return parsed if parsed >= 0 else default

        output: list[dict[str, object]] = []
        for index, item in enumerate(value, start=1):
            if not isinstance(item, dict):
                continue

            start_url = str(item.get("start_url", "")).strip()
            if not start_url:
                continue

            recursive = bool(item.get("recursive", True))
            max_depth = _nonnegative_int(item.get("max_depth", 2), 2)
            if not recursive:
                max_depth = 0

            output.append(
                {
                    "name": str(item.get("name", "")).strip() or f"Web Discovery {index}",
                    "start_url": start_url,
                    "scan_interval_minutes": _positive_int(item.get("scan_interval_minutes", 60), 60),
                    "recursive": recursive,
                    "max_depth": max_depth,
                    "max_pages": _positive_int(item.get("max_pages", 150), 150),
                    "include_subdomains": bool(item.get("include_subdomains", False)),
                    "follow_robots": bool(item.get("follow_robots", True)),
                    "source_concurrency": _positive_int(item.get("source_concurrency", 1), 1),
                    "enabled": bool(item.get("enabled", True)),
                }
            )

        return output

    def _refresh_sources_tree(self, select_index: int | None = None) -> None:
        if not hasattr(self, "sources_tree"):
            return

        for item_id in self.sources_tree.get_children():
            self.sources_tree.delete(item_id)

        for index, source in enumerate(self.sources_items, start=1):
            concurrency = str(source.get("concurrency", "1")).strip() or "1"
            self.sources_tree.insert(
                "",
                tk.END,
                iid=str(index - 1),
                values=(index, source.get("name", ""), source.get("url", ""), concurrency),
            )

        if select_index is None:
            return
        if 0 <= select_index < len(self.sources_items):
            item_id = str(select_index)
            self.sources_tree.selection_set(item_id)
            self.sources_tree.focus(item_id)
            self.sources_tree.see(item_id)

    def _selected_source_index(self) -> int | None:
        selected = self.sources_tree.selection()
        if not selected:
            return None

        try:
            index = int(selected[0])
        except ValueError:
            return None

        if index < 0 or index >= len(self.sources_items):
            return None
        return index

    def _on_source_tree_select(self, event: object | None = None) -> None:
        del event
        index = self._selected_source_index()
        if index is None:
            return
        source = self.sources_items[index]
        self.source_name_var.set(str(source.get("name", "")).strip())
        self.source_url_var.set(str(source.get("url", "")).strip())

    def _clear_source_editor(self) -> None:
        self.source_name_var.set("")
        self.source_url_var.set("")
        for item_id in self.sources_tree.selection():
            self.sources_tree.selection_remove(item_id)

    def _open_add_source_dialog(self) -> None:
        popup = tk.Toplevel(self)
        popup.title("Add Source")
        popup.geometry("520x260")
        popup.minsize(460, 220)
        popup.transient(self)
        popup.grab_set()

        frame = ttk.Frame(popup, padding=12)
        frame.pack(fill=tk.BOTH, expand=True)
        frame.columnconfigure(0, weight=0)
        frame.columnconfigure(1, weight=1)

        name_var = tk.StringVar(value=f"Source {len(self.sources_items) + 1}")
        url_var = tk.StringVar()
        concurrency_var = tk.StringVar(value="1")

        ttk.Label(frame, text="Source Name").grid(row=0, column=0, sticky="w")
        name_entry = ttk.Entry(frame, textvariable=name_var)
        name_entry.grid(row=0, column=1, sticky="ew", padx=(8, 0))

        ttk.Label(frame, text="Source URL").grid(row=1, column=0, sticky="w", pady=(10, 0))
        url_entry = ttk.Entry(frame, textvariable=url_var)
        url_entry.grid(row=1, column=1, sticky="ew", padx=(8, 0), pady=(10, 0))

        ttk.Label(frame, text="Max Concurrency").grid(row=2, column=0, sticky="w", pady=(10, 0))
        concurrency_entry = ttk.Entry(frame, textvariable=concurrency_var, width=8)
        concurrency_entry.grid(row=2, column=1, sticky="w", padx=(8, 0), pady=(10, 0))

        ttk.Label(
            frame,
            text="Source index is assigned automatically by list order.\nUse up/down icons to reorder after adding.",
            foreground="#4f4f4f",
        ).grid(row=3, column=0, columnspan=2, sticky="w", pady=(10, 0))

        footer = ttk.Frame(frame)
        footer.grid(row=4, column=0, columnspan=2, sticky="e", pady=(14, 0))

        def add_source_from_dialog() -> None:
            name = name_var.get().strip() or f"Source {len(self.sources_items) + 1}"
            url = url_var.get().strip()
            concurrency = concurrency_var.get().strip() or "1"

            if not url:
                self._show_error("Invalid Source", "Source URL is required.")
                return
            if not concurrency.isdigit() or int(concurrency) < 1:
                self._show_error("Invalid Source", "Max concurrency must be a positive integer.")
                return

            self.sources_items.append({"name": name, "url": url, "concurrency": concurrency})
            new_index = len(self.sources_items) - 1
            self._refresh_sources_tree(select_index=new_index)
            self._set_last_event(f"Added source '{name}'.")
            popup.destroy()

        ttk.Button(footer, text="Cancel", command=popup.destroy).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(footer, text="Add Source", command=add_source_from_dialog).pack(side=tk.LEFT)

        popup.bind("<Return>", lambda event: add_source_from_dialog())
        popup.bind("<Escape>", lambda event: popup.destroy())
        name_entry.focus_set()
        name_entry.selection_range(0, tk.END)

    def _add_source_item_clicked(self) -> None:
        # Backward-compatible wrapper for any stale command references.
        self._open_add_source_dialog()

    def _update_source_item_clicked(self) -> None:
        index = self._selected_source_index()
        if index is None:
            self._show_error("No Source Selected", "Select a source row to update.")
            return

        url = self.source_url_var.get().strip()
        if not url:
            self._show_error("Invalid Source", "Source URL is required.")
            return

        name = self.source_name_var.get().strip() or f"Source {index + 1}"
        self.sources_items[index]["name"] = name
        self.sources_items[index]["url"] = url
        concurrency = str(self.sources_items[index].get("concurrency", "1")).strip() or "1"
        if not concurrency.isdigit() or int(concurrency) < 1:
            concurrency = "1"
        self.sources_items[index]["concurrency"] = concurrency

        self._refresh_sources_tree(select_index=index)
        self._set_last_event(f"Updated source '{name}'.")

    def _remove_source_item_clicked(self) -> None:
        index = self._selected_source_index()
        if index is None:
            self._show_error("No Source Selected", "Select a source row to remove.")
            return

        removed = self.sources_items.pop(index)
        next_index: int | None = None
        if self.sources_items:
            next_index = min(index, len(self.sources_items) - 1)
        self._refresh_sources_tree(select_index=next_index)
        if next_index is None:
            self._clear_source_editor()
        self._set_last_event(f"Removed source '{removed.get('name', f'Source {index + 1}')}'.")

    def _move_source_up_clicked(self) -> None:
        index = self._selected_source_index()
        if index is None:
            self._show_error("No Source Selected", "Select a source row to move.")
            return
        if index == 0:
            return

        self.sources_items[index - 1], self.sources_items[index] = self.sources_items[index], self.sources_items[index - 1]
        self._refresh_sources_tree(select_index=index - 1)
        moved_name = self.sources_items[index - 1].get("name", f"Source {index}")
        self._set_last_event(f"Moved source '{moved_name}' to index {index}.")
        self._reassign_channels_for_source_order_change()

    def _move_source_down_clicked(self) -> None:
        index = self._selected_source_index()
        if index is None:
            self._show_error("No Source Selected", "Select a source row to move.")
            return
        if index >= len(self.sources_items) - 1:
            return

        self.sources_items[index], self.sources_items[index + 1] = self.sources_items[index + 1], self.sources_items[index]
        self._refresh_sources_tree(select_index=index + 1)
        moved_name = self.sources_items[index + 1].get("name", f"Source {index + 1}")
        self._set_last_event(f"Moved source '{moved_name}' to index {index + 2}.")
        self._reassign_channels_for_source_order_change()

    def _reassign_channels_for_source_order_change(self) -> None:
        refresh_callback = self.channel_source_reorder_callback
        if refresh_callback is not None:
            try:
                refresh_callback()
            except Exception as exc:  # noqa: BLE001
                self._append_log(f"[APP] Source reorder refresh failed: {exc}")

        self._append_log("[APP] Source order changed. Reassigning channel source order (.1, .2, ...).")
        try:
            settings = self._collect_settings()
            self._persist_settings(settings, sync_start_on_boot=True, show_dialogs=False, log_event=False)
        except Exception as exc:  # noqa: BLE001
            self._append_log(f"[APP] Source reorder saved in UI only: {exc}")
            return

        self._restart_server_if_running("Source order changed.")

    def _open_web_discovery_popup(self) -> None:
        popup = tk.Toplevel(self)
        popup.title("Web Discovery")
        popup.geometry("1040x640")
        popup.minsize(920, 560)
        popup.transient(self)
        popup.grab_set()

        jobs: list[dict[str, object]] = [dict(job) for job in self.web_discovery_jobs]

        root = ttk.Frame(popup, padding=12)
        root.pack(fill=tk.BOTH, expand=True)
        root.columnconfigure(0, weight=1)
        root.rowconfigure(1, weight=1)

        ttk.Label(
            root,
            text=(
                "Scan a site for M3U and M3U8 playlists using the backend discovery engine. "
                "Jobs can recurse through same-site links, read sitemaps, and rescan on their own interval."
            ),
            wraplength=980,
            justify=tk.LEFT,
        ).grid(row=0, column=0, sticky="ew")

        content = ttk.Frame(root)
        content.grid(row=1, column=0, sticky="nsew", pady=(12, 0))
        content.columnconfigure(0, weight=1)
        content.rowconfigure(0, weight=1)

        tree_frame = ttk.Frame(content)
        tree_frame.grid(row=0, column=0, sticky="nsew")
        tree_frame.columnconfigure(0, weight=1)
        tree_frame.rowconfigure(0, weight=1)

        jobs_tree = ttk.Treeview(
            tree_frame,
            columns=("name", "start_url", "interval", "mode", "status"),
            show="headings",
            selectmode="browse",
            height=14,
        )
        jobs_tree.heading("name", text="Job Name")
        jobs_tree.heading("start_url", text="Start URL")
        jobs_tree.heading("interval", text="Interval")
        jobs_tree.heading("mode", text="Mode")
        jobs_tree.heading("status", text="Status")
        jobs_tree.column("name", width=180, anchor=tk.W, stretch=False)
        jobs_tree.column("start_url", width=460, anchor=tk.W, stretch=True)
        jobs_tree.column("interval", width=90, anchor=tk.CENTER, stretch=False)
        jobs_tree.column("mode", width=110, anchor=tk.CENTER, stretch=False)
        jobs_tree.column("status", width=90, anchor=tk.CENTER, stretch=False)
        jobs_tree.grid(row=0, column=0, sticky="nsew")
        jobs_scroll_y = ttk.Scrollbar(tree_frame, orient=tk.VERTICAL, command=jobs_tree.yview)
        jobs_scroll_y.grid(row=0, column=1, sticky="ns")
        jobs_tree.configure(yscrollcommand=jobs_scroll_y.set)

        summary_var = tk.StringVar(value="No discovery jobs configured yet.")
        ttk.Label(root, textvariable=summary_var, wraplength=980, justify=tk.LEFT).grid(
            row=2, column=0, sticky="ew", pady=(10, 0)
        )

        footer = ttk.Frame(root)
        footer.grid(row=3, column=0, sticky="ew", pady=(14, 0))

        def selected_index() -> int | None:
            selected = jobs_tree.selection()
            if not selected:
                return None
            try:
                index = int(selected[0])
            except ValueError:
                return None
            if 0 <= index < len(jobs):
                return index
            return None

        def describe_job(job: dict[str, object]) -> str:
            mode = "recursive" if bool(job.get("recursive", True)) else "single page"
            subdomains = "with subdomains" if bool(job.get("include_subdomains", False)) else "same host only"
            robots_text = "respecting robots.txt" if bool(job.get("follow_robots", True)) else "ignoring robots.txt"
            enabled_text = "enabled" if bool(job.get("enabled", True)) else "disabled"
            return (
                f"{job.get('name', 'Web Discovery')} scans {job.get('start_url', '')} every "
                f"{job.get('scan_interval_minutes', 60)} minute(s), runs in {mode} mode, crawls {subdomains}, "
                f"stops after {job.get('max_pages', 150)} pages, and is currently {enabled_text} while {robots_text}."
            )

        def refresh_tree(select_index: int | None = None) -> None:
            for item_id in jobs_tree.get_children():
                jobs_tree.delete(item_id)

            for index, job in enumerate(jobs):
                interval = f"{job.get('scan_interval_minutes', 60)} min"
                mode = "Recursive" if bool(job.get("recursive", True)) else "Single Page"
                status = "Enabled" if bool(job.get("enabled", True)) else "Disabled"
                jobs_tree.insert(
                    "",
                    tk.END,
                    iid=str(index),
                    values=(
                        str(job.get("name", "")).strip(),
                        str(job.get("start_url", "")).strip(),
                        interval,
                        mode,
                        status,
                    ),
                )

            if select_index is not None and 0 <= select_index < len(jobs):
                item_id = str(select_index)
                jobs_tree.selection_set(item_id)
                jobs_tree.focus(item_id)
                jobs_tree.see(item_id)

            index = selected_index()
            if index is None:
                summary_var.set("No discovery job selected. Add one to start scanning a site for M3U links.")
                return
            summary_var.set(describe_job(jobs[index]))

        def open_job_editor(edit_index: int | None = None) -> None:
            job = dict(jobs[edit_index]) if edit_index is not None and 0 <= edit_index < len(jobs) else {}
            editor = tk.Toplevel(popup)
            editor.title("Edit Discovery Job" if edit_index is not None else "Add Discovery Job")
            editor.geometry("640x420")
            editor.minsize(560, 380)
            editor.transient(popup)
            editor.grab_set()

            frame = ttk.Frame(editor, padding=12)
            frame.pack(fill=tk.BOTH, expand=True)
            frame.columnconfigure(1, weight=1)

            name_var = tk.StringVar(value=str(job.get("name", "")).strip() or f"Web Discovery {len(jobs) + 1}")
            start_url_var = tk.StringVar(value=str(job.get("start_url", "")).strip())
            interval_var = tk.StringVar(value=str(job.get("scan_interval_minutes", 60)))
            recursive_var = tk.BooleanVar(value=bool(job.get("recursive", True) if job else True))
            max_depth_var = tk.StringVar(value=str(job.get("max_depth", 2 if recursive_var.get() else 0)))
            max_pages_var = tk.StringVar(value=str(job.get("max_pages", 150)))
            subdomains_var = tk.BooleanVar(value=bool(job.get("include_subdomains", False)))
            robots_var = tk.BooleanVar(value=bool(job.get("follow_robots", True) if job else True))
            source_concurrency_var = tk.StringVar(value=str(job.get("source_concurrency", 1)))
            enabled_var = tk.BooleanVar(value=bool(job.get("enabled", True) if job else True))

            ttk.Label(frame, text="Job Name").grid(row=0, column=0, sticky="w")
            name_entry = ttk.Entry(frame, textvariable=name_var)
            name_entry.grid(row=0, column=1, sticky="ew", padx=(8, 0))

            ttk.Label(frame, text="Start URL").grid(row=1, column=0, sticky="w", pady=(10, 0))
            start_url_entry = ttk.Entry(frame, textvariable=start_url_var)
            start_url_entry.grid(row=1, column=1, sticky="ew", padx=(8, 0), pady=(10, 0))

            ttk.Label(frame, text="Scan Interval (min)").grid(row=2, column=0, sticky="w", pady=(10, 0))
            ttk.Entry(frame, textvariable=interval_var, width=10).grid(row=2, column=1, sticky="w", padx=(8, 0), pady=(10, 0))

            ttk.Label(frame, text="Max Depth").grid(row=3, column=0, sticky="w", pady=(10, 0))
            max_depth_entry = ttk.Entry(frame, textvariable=max_depth_var, width=10)
            max_depth_entry.grid(row=3, column=1, sticky="w", padx=(8, 0), pady=(10, 0))

            ttk.Label(frame, text="Max Pages").grid(row=4, column=0, sticky="w", pady=(10, 0))
            ttk.Entry(frame, textvariable=max_pages_var, width=10).grid(row=4, column=1, sticky="w", padx=(8, 0), pady=(10, 0))

            ttk.Label(frame, text="Discovered Source Concurrency").grid(row=5, column=0, sticky="w", pady=(10, 0))
            ttk.Entry(frame, textvariable=source_concurrency_var, width=10).grid(
                row=5, column=1, sticky="w", padx=(8, 0), pady=(10, 0)
            )

            checks = ttk.Frame(frame)
            checks.grid(row=6, column=0, columnspan=2, sticky="w", pady=(14, 0))
            ttk.Checkbutton(checks, text="Enabled", variable=enabled_var).grid(row=0, column=0, sticky="w", padx=(0, 12))
            ttk.Checkbutton(checks, text="Recursive Crawl", variable=recursive_var).grid(
                row=0, column=1, sticky="w", padx=(0, 12)
            )
            ttk.Checkbutton(checks, text="Include Subdomains", variable=subdomains_var).grid(
                row=0, column=2, sticky="w", padx=(0, 12)
            )
            ttk.Checkbutton(checks, text="Respect robots.txt", variable=robots_var).grid(row=0, column=3, sticky="w")

            ttk.Label(
                frame,
                text=(
                    "Recursive crawl follows same-site links. Max Depth counts link levels from the start URL. "
                    "Sitemap discovery runs automatically when available."
                ),
                wraplength=600,
                foreground="#4f4f4f",
                justify=tk.LEFT,
            ).grid(row=7, column=0, columnspan=2, sticky="w", pady=(12, 0))

            def sync_depth_state() -> None:
                if recursive_var.get():
                    max_depth_entry.state(["!disabled"])
                    if not str(max_depth_var.get()).strip():
                        max_depth_var.set("2")
                else:
                    max_depth_entry.state(["disabled"])
                    max_depth_var.set("0")

            def save_job() -> None:
                start_url = start_url_var.get().strip()
                if not start_url:
                    self._show_error("Invalid Discovery Job", "Start URL is required.")
                    return

                parsed = urllib.parse.urlparse(start_url)
                if parsed.scheme not in {"http", "https"} or not parsed.netloc:
                    self._show_error("Invalid Discovery Job", "Start URL must be a valid http or https URL.")
                    return

                try:
                    interval = int(interval_var.get().strip() or "60")
                    max_depth = int(max_depth_var.get().strip() or "0")
                    max_pages = int(max_pages_var.get().strip() or "150")
                    source_concurrency = int(source_concurrency_var.get().strip() or "1")
                except ValueError:
                    self._show_error("Invalid Discovery Job", "Interval, depth, pages, and concurrency must be integers.")
                    return

                if interval < 1:
                    self._show_error("Invalid Discovery Job", "Scan interval must be at least 1 minute.")
                    return
                if max_depth < 0:
                    self._show_error("Invalid Discovery Job", "Max depth cannot be negative.")
                    return
                if max_pages < 1:
                    self._show_error("Invalid Discovery Job", "Max pages must be at least 1.")
                    return
                if source_concurrency < 1:
                    self._show_error("Invalid Discovery Job", "Source concurrency must be at least 1.")
                    return

                recursive = bool(recursive_var.get())
                job_payload: dict[str, object] = {
                    "name": name_var.get().strip() or f"Web Discovery {len(jobs) + 1}",
                    "start_url": start_url,
                    "scan_interval_minutes": interval,
                    "recursive": recursive,
                    "max_depth": max_depth if recursive else 0,
                    "max_pages": max_pages,
                    "include_subdomains": bool(subdomains_var.get()),
                    "follow_robots": bool(robots_var.get()),
                    "source_concurrency": source_concurrency,
                    "enabled": bool(enabled_var.get()),
                }

                normalized = self._normalize_web_discovery_jobs([job_payload])
                if not normalized:
                    self._show_error("Invalid Discovery Job", "This job could not be saved. Check the configured values.")
                    return

                if edit_index is None:
                    jobs.append(normalized[0])
                    select_index = len(jobs) - 1
                else:
                    jobs[edit_index] = normalized[0]
                    select_index = edit_index

                refresh_tree(select_index=select_index)
                editor.destroy()

            footer_row = ttk.Frame(frame)
            footer_row.grid(row=8, column=0, columnspan=2, sticky="e", pady=(18, 0))
            ttk.Button(footer_row, text="Cancel", command=editor.destroy).pack(side=tk.LEFT, padx=(0, 8))
            ttk.Button(footer_row, text="Save Job", command=save_job).pack(side=tk.LEFT)

            recursive_var.trace_add("write", lambda *_args: sync_depth_state())
            sync_depth_state()
            editor.bind("<Return>", lambda event: save_job())
            editor.bind("<Escape>", lambda event: editor.destroy())
            name_entry.focus_set()
            name_entry.selection_range(0, tk.END)

        def add_job() -> None:
            open_job_editor()

        def edit_job() -> None:
            index = selected_index()
            if index is None:
                self._show_error("No Discovery Job Selected", "Select a discovery job to edit.")
                return
            open_job_editor(edit_index=index)

        def remove_job() -> None:
            index = selected_index()
            if index is None:
                self._show_error("No Discovery Job Selected", "Select a discovery job to remove.")
                return
            removed = jobs.pop(index)
            refresh_tree(select_index=min(index, len(jobs) - 1) if jobs else None)
            self._set_last_event(f"Removed discovery job '{removed.get('name', 'Web Discovery')}'.")

        actions = ttk.Frame(footer)
        actions.pack(side=tk.LEFT)
        ttk.Button(actions, text="Add Job", command=add_job).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(actions, text="Edit Job", command=edit_job).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(actions, text="Remove Job", command=remove_job).pack(side=tk.LEFT)

        def save_popup_settings() -> None:
            self.web_discovery_jobs = self._normalize_web_discovery_jobs(jobs)

            try:
                settings = self._collect_settings()
                self._persist_settings(settings, sync_start_on_boot=True, show_dialogs=False, log_event=False)
            except Exception as exc:  # noqa: BLE001
                self._show_error("Invalid Settings", str(exc))
                return

            self._append_log("[APP] Web discovery settings saved.")
            self._restart_server_if_running("Web discovery settings changed.")
            popup.destroy()

        footer_buttons = ttk.Frame(footer)
        footer_buttons.pack(side=tk.RIGHT)
        ttk.Button(footer_buttons, text="Cancel", command=popup.destroy).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(footer_buttons, text="Save", command=save_popup_settings).pack(side=tk.LEFT)

        jobs_tree.bind("<<TreeviewSelect>>", lambda event: refresh_tree())
        jobs_tree.bind("<Double-1>", lambda event: edit_job())
        refresh_tree(select_index=0 if jobs else None)

    def _normalize_channel_source_rules(self, value: object) -> list[dict[str, object]]:
        if not isinstance(value, list):
            return []

        output: list[dict[str, object]] = []
        for item in value:
            if not isinstance(item, dict):
                continue

            pattern = str(item.get("pattern", "")).strip()
            raw_sources = item.get("sources", [])
            if not pattern or not isinstance(raw_sources, list):
                continue

            sources: list[str] = []
            for source in raw_sources:
                source_index = str(source).strip()
                if not source_index.isdigit() or int(source_index) < 1:
                    continue
                if source_index not in sources:
                    sources.append(source_index)

            if not sources:
                continue

            output.append({"pattern": pattern, "sources": sources})

        return output

    def _normalize_channel_merge_rules(self, value: object) -> list[dict[str, str]]:
        if not isinstance(value, list):
            return []

        output: list[dict[str, str]] = []
        seen_sources: set[str] = set()
        for item in value:
            if not isinstance(item, dict):
                continue

            source = str(item.get("source", "")).strip()
            target = str(item.get("target", "")).strip()
            if not source or not target:
                continue
            if source.casefold() == target.casefold():
                continue

            source_key = source.casefold()
            if source_key in seen_sources:
                continue
            seen_sources.add(source_key)

            output.append({"source": source, "target": target})

        return output

    def _channel_name_to_exact_pattern(self, channel: str) -> str:
        return f"^{re.escape(channel)}$"

    def _channel_discovery_key(self, channel: str) -> str:
        normalized = " ".join(str(channel).strip().split()).lower()
        if not normalized:
            return ""

        for old, new in (
            ("/", "_"),
            ("\\", "_"),
            (":", "_"),
            ("*", "_"),
            ("?", "_"),
            ('"', "_"),
            ("<", "_"),
            (">", "_"),
            ("|", "_"),
            (" ", ""),
        ):
            normalized = normalized.replace(old, new)
        return normalized[:100]

    def _channel_name_to_runtime_merge_exclude_pattern(self, channel: str) -> str:
        normalized = " ".join(str(channel).strip().split())
        if not normalized:
            return ""
        tokens = [re.escape(token) for token in normalized.split(" ")]
        return rf"(?i)^\s*{r'\s+'.join(tokens)}\s*$"

    def _pattern_to_channel_name(self, pattern: str) -> str | None:
        raw = pattern.strip()
        if not raw:
            return None

        # Exact literal pattern produced by this app: ^<escaped_name>$
        if raw.startswith("^") and raw.endswith("$"):
            body = raw[1:-1]
            channel_chars: list[str] = []
            i = 0
            while i < len(body):
                char = body[i]
                if char == "\\":
                    if i + 1 >= len(body):
                        return None
                    channel_chars.append(body[i + 1])
                    i += 2
                    continue
                if char in ".^$*+?{}[]|()":
                    return None
                channel_chars.append(char)
                i += 1
            channel = "".join(channel_chars).strip()
            return channel or None

        # Support simple plain-text patterns from older settings.
        if re.search(r"[.^$*+?{}\[\]|()]", raw) is None:
            return raw
        return None

    def _discover_channels_from_sources(
        self, sources: list[dict[str, str]]
    ) -> tuple[list[str], dict[str, list[str]], list[str]]:
        deduped: dict[str, str] = {}
        channel_sources: dict[str, list[str]] = {}
        errors: list[str] = []
        for source_index, source in enumerate(sources, start=1):
            source_name = str(source.get("name", "")).strip() or f"Source {source_index}"
            source_url = str(source.get("url", "")).strip()
            if not source_url:
                continue
            try:
                content = self._read_m3u_source_text(source_url)
                for channel in self._extract_channel_titles_from_text(content):
                    key = self._channel_discovery_key(channel)
                    if not key:
                        continue
                    if key not in deduped:
                        deduped[key] = channel
                    if key not in channel_sources:
                        channel_sources[key] = []
                    if source_name not in channel_sources[key]:
                        channel_sources[key].append(source_name)
            except Exception as exc:  # noqa: BLE001
                errors.append(f"Channel discovery skipped '{source_name}': {exc}")

        channels = sorted(deduped.values(), key=lambda v: v.casefold())
        source_by_channel: dict[str, list[str]] = {}
        for channel in channels:
            key = self._channel_discovery_key(channel)
            source_by_channel[channel] = list(channel_sources.get(key, []))

        return channels, source_by_channel, errors

    def _read_m3u_source_text(self, source_url: str) -> str:
        if re.match(r"^[A-Za-z]:[\\/]", source_url):
            file_path = Path(source_url)
            if not file_path.exists():
                raise FileNotFoundError(f"File not found: {file_path}")
            data = file_path.read_bytes()
        else:
            parsed = urllib.parse.urlparse(source_url)
            if parsed.scheme in {"http", "https", "file"}:
                request = urllib.request.Request(
                    source_url,
                    headers={"User-Agent": "WindowsM3UStreamMergerProxyDesktop/1.0"},
                )
                with urllib.request.urlopen(request, timeout=15) as response:
                    data = response.read()
            elif parsed.scheme == "":
                file_path = Path(source_url)
                if not file_path.exists():
                    raise ValueError("Invalid source URL or file path.")
                data = file_path.read_bytes()
            else:
                raise ValueError(f"Unsupported source scheme: {parsed.scheme}")

        for encoding in ("utf-8-sig", "utf-8", "latin-1"):
            try:
                return data.decode(encoding)
            except UnicodeDecodeError:
                continue
        return data.decode("utf-8", errors="ignore")

    def _extract_channel_titles_from_text(self, content: str) -> list[str]:
        channels: list[str] = []
        attribute_pattern = re.compile(r'([A-Za-z0-9_-]+)="([^"]*)"')
        for line in content.splitlines():
            raw = line.strip()
            if not raw.startswith("#EXTINF"):
                continue

            title = ""
            for key, value in attribute_pattern.findall(raw):
                if key.strip().lower() == "tvg-name":
                    title = value.strip()
                    break

            if "," in raw:
                display_title = raw.rsplit(",", 1)[1].strip()
                if display_title:
                    title = display_title

            title = " ".join(title.split())
            if title:
                channels.append(title)
        return channels

    def _open_channel_settings_popup(self) -> None:
        popup = tk.Toplevel(self)
        popup.title("Channel Settings")
        popup.geometry("980x700")
        popup.minsize(860, 620)
        popup.transient(self)
        popup.grab_set()

        root = ttk.Frame(popup, padding=12)
        root.pack(fill=tk.BOTH, expand=True)
        root.columnconfigure(0, weight=1)
        root.rowconfigure(2, weight=1)
        root.rowconfigure(4, weight=1)

        include_unmapped_filters: list[str] = []
        exclude_unmapped_filters: list[str] = []
        include_channels: list[str] = []
        exclude_channels: list[str] = []
        for pattern in self.include_title_filters:
            channel = self._pattern_to_channel_name(pattern)
            if channel is None:
                include_unmapped_filters.append(pattern)
            elif channel not in include_channels:
                include_channels.append(channel)
        for pattern in self.exclude_title_filters:
            channel = self._pattern_to_channel_name(pattern)
            if channel is None:
                exclude_unmapped_filters.append(pattern)
            elif channel not in exclude_channels:
                exclude_channels.append(channel)

        loaded_channels: list[str] = []
        loaded_channel_sources: dict[str, list[str]] = {}
        merge_map: dict[str, str] = {}
        for rule in self.channel_merge_rules:
            source = str(rule.get("source", "")).strip()
            target = str(rule.get("target", "")).strip()
            if not source or not target:
                continue
            if source.casefold() == target.casefold():
                continue
            merge_map[source.casefold()] = target

        channel_status_var = tk.StringVar(value="Click Refresh Channels to load channels from all sources.")

        channel_row = ttk.Frame(root)
        channel_row.grid(row=0, column=0, sticky="ew")
        channel_row.columnconfigure(0, weight=1)
        ttk.Label(
            channel_row,
            text="Channels are loaded from all sources and numbered automatically (A-Z).",
            foreground="#4f4f4f",
        ).grid(row=0, column=0, sticky="w")
        refresh_channels_button = ttk.Button(channel_row, text="Refresh Channels")
        refresh_channels_button.grid(row=0, column=1, sticky="e")

        ttk.Label(root, textvariable=channel_status_var, foreground="#4f4f4f").grid(
            row=1, column=0, sticky="w", pady=(8, 8)
        )

        lists_frame = ttk.Frame(root)
        lists_frame.grid(row=2, column=0, sticky="nsew")
        lists_frame.columnconfigure(0, weight=1)
        lists_frame.columnconfigure(1, weight=0)
        lists_frame.columnconfigure(2, weight=1)
        lists_frame.rowconfigure(0, weight=1)

        include_frame = ttk.LabelFrame(lists_frame, text="Included Channels", padding=8)
        include_frame.grid(row=0, column=0, sticky="nsew", padx=(0, 4))
        include_frame.columnconfigure(0, weight=1)
        include_frame.rowconfigure(0, weight=1)
        include_listbox = tk.Listbox(include_frame, exportselection=False, selectmode=tk.EXTENDED)
        include_listbox.grid(row=0, column=0, sticky="nsew")
        include_scroll = ttk.Scrollbar(include_frame, orient=tk.VERTICAL, command=include_listbox.yview)
        include_scroll.grid(row=0, column=1, sticky="ns")
        include_listbox.configure(yscrollcommand=include_scroll.set)

        transfer_mid = ttk.Frame(lists_frame)
        transfer_mid.grid(row=0, column=1, sticky="ns")
        transfer_mid.rowconfigure(0, weight=1)
        transfer_mid.rowconfigure(1, weight=0)
        transfer_mid.rowconfigure(2, weight=1)
        transfer_mid.rowconfigure(3, weight=0)
        transfer_icon = self._create_channel_transfer_icon()
        transfer_button = ttk.Button(transfer_mid, image=transfer_icon, width=2)
        transfer_button.image = transfer_icon
        transfer_button.grid(row=1, column=0, padx=6)
        transfer_button.state(["disabled"])
        merge_selected_button = ttk.Button(transfer_mid, text="Merge Selected...", width=16)
        merge_selected_button.grid(row=3, column=0, padx=6, pady=(8, 0), sticky="n")
        merge_selected_button.state(["disabled"])

        exclude_frame = ttk.LabelFrame(lists_frame, text="Excluded Channels", padding=8)
        exclude_frame.grid(row=0, column=2, sticky="nsew", padx=(4, 0))
        exclude_frame.columnconfigure(0, weight=1)
        exclude_frame.rowconfigure(0, weight=1)
        exclude_listbox = tk.Listbox(exclude_frame, exportselection=False, selectmode=tk.EXTENDED)
        exclude_listbox.grid(row=0, column=0, sticky="nsew")
        exclude_scroll = ttk.Scrollbar(exclude_frame, orient=tk.VERTICAL, command=exclude_listbox.yview)
        exclude_scroll.grid(row=0, column=1, sticky="ns")
        exclude_listbox.configure(yscrollcommand=exclude_scroll.set)

        numbering_frame = ttk.LabelFrame(root, text="Channel Numbers (Auto)", padding=8)
        numbering_frame.grid(row=4, column=0, sticky="nsew", pady=(10, 0))
        numbering_frame.columnconfigure(0, weight=1)
        numbering_frame.rowconfigure(0, weight=1)

        channel_number_tree = ttk.Treeview(
            numbering_frame,
            columns=("number",),
            show="tree headings",
            selectmode="browse",
        )
        channel_number_tree.heading("#0", text="Channel")
        channel_number_tree.heading("number", text="#")
        channel_number_tree.column("#0", width=700, anchor=tk.W, stretch=True)
        channel_number_tree.column("number", width=70, anchor=tk.CENTER, stretch=False)
        channel_number_tree.grid(row=0, column=0, sticky="nsew")
        numbering_scroll = ttk.Scrollbar(numbering_frame, orient=tk.VERTICAL, command=channel_number_tree.yview)
        numbering_scroll.grid(row=0, column=1, sticky="ns")
        channel_number_tree.configure(yscrollcommand=numbering_scroll.set)

        footer = ttk.Frame(root)
        footer.grid(row=5, column=0, sticky="e", pady=(10, 0))
        ttk.Button(footer, text="Cancel", command=popup.destroy).pack(side=tk.LEFT, padx=(0, 8))
        save_button = ttk.Button(footer, text="Save")
        save_button.pack(side=tk.LEFT)

        context_channel: dict[str, str] = {"name": ""}
        drag_state: dict[str, object] = {"channel": "", "x_root": 0, "y_root": 0}
        context_menu = tk.Menu(self, tearoff=0)
        include_listbox_channels: list[str] = []
        exclude_listbox_channels: list[str] = []
        channel_number_item_channel: dict[str, str] = {}
        source_priority_by_name: dict[str, int] = {}

        def rebuild_source_priority_map() -> None:
            source_priority_by_name.clear()
            for source_index, source in enumerate(self.sources_items, start=1):
                source_name = str(source.get("name", "")).strip() or f"Source {source_index}"
                source_key = source_name.casefold()
                if source_key not in source_priority_by_name:
                    source_priority_by_name[source_key] = source_index

        rebuild_source_priority_map()

        def refresh_transfer_state() -> None:
            if loaded_channels:
                transfer_button.state(["!disabled"])
                merge_selected_button.state(["!disabled"])
            else:
                transfer_button.state(["disabled"])
                merge_selected_button.state(["disabled"])

        def normalize_merge_map_for_loaded_channels() -> None:
            if not loaded_channels:
                merge_map.clear()
                return

            channel_name_by_key = {channel.casefold(): channel for channel in loaded_channels}
            raw_edges: dict[str, str] = {}
            for source_key, target_name in merge_map.items():
                target_key = target_name.casefold()
                if source_key not in channel_name_by_key:
                    continue
                if target_key not in channel_name_by_key:
                    continue
                if source_key == target_key:
                    continue
                raw_edges[source_key] = target_key

            resolved_edges: dict[str, str] = {}
            for source_key in sorted(raw_edges.keys()):
                seen: set[str] = {source_key}
                current_key = raw_edges[source_key]
                while current_key in raw_edges and current_key not in seen:
                    seen.add(current_key)
                    current_key = raw_edges[current_key]
                if current_key in seen:
                    continue
                if source_key != current_key:
                    resolved_edges[source_key] = current_key

            merge_map.clear()
            for source_key, target_key in resolved_edges.items():
                merge_map[source_key] = channel_name_by_key[target_key]

        def resolve_channel_name(channel: str) -> str:
            if not channel:
                return channel

            channel_name_by_key = {item.casefold(): item for item in loaded_channels}
            current_key = channel.casefold()
            if current_key not in channel_name_by_key:
                return channel

            seen: set[str] = set()
            while current_key in merge_map:
                if current_key in seen:
                    break
                seen.add(current_key)
                next_key = str(merge_map[current_key]).strip().casefold()
                if not next_key or next_key not in channel_name_by_key:
                    break
                current_key = next_key

            return channel_name_by_key.get(current_key, channel)

        def sort_source_names(values: list[str]) -> list[str]:
            rebuild_source_priority_map()
            deduped: dict[str, str] = {}
            for value in values:
                source_name = str(value).strip()
                if not source_name:
                    continue
                source_key = source_name.casefold()
                if source_key not in deduped:
                    deduped[source_key] = source_name

            return sorted(
                deduped.values(),
                key=lambda name: (source_priority_by_name.get(name.casefold(), 10_000), name.casefold()),
            )

        def channel_source_names(channel: str, aggregate: bool = False) -> list[str]:
            raw_channel = channel.strip()
            if not raw_channel:
                return []

            if not aggregate:
                return sort_source_names(list(loaded_channel_sources.get(raw_channel, [])))

            target_key = resolve_channel_name(raw_channel).casefold()
            source_names: list[str] = []
            for candidate in loaded_channels:
                if resolve_channel_name(candidate).casefold() != target_key:
                    continue
                source_names.extend(loaded_channel_sources.get(candidate, []))

            if not source_names:
                source_names.extend(loaded_channel_sources.get(raw_channel, []))

            return sort_source_names(source_names)

        def format_channel_label(channel: str, aggregate: bool = False) -> str:
            names = channel_source_names(channel, aggregate=aggregate)
            if not names:
                return f"{channel} (Unknown Source)"
            return f"{channel} ({', '.join(names)})"

        def refresh_channel_listbox(box: tk.Listbox, values: list[str], channel_store: list[str]) -> None:
            channel_store[:] = sorted(values, key=lambda value: value.casefold())
            box.delete(0, tk.END)
            for channel in channel_store:
                box.insert(tk.END, format_channel_label(channel, aggregate=True))

        def selected_channels_from_listbox(box: tk.Listbox, channel_store: list[str]) -> list[str]:
            selected: list[str] = []
            for index in box.curselection():
                if 0 <= index < len(channel_store):
                    selected.append(channel_store[index])
            return selected

        def channel_source_assignments(parent_channel: str) -> list[tuple[str, str]]:
            canonical_channel = resolve_channel_name(parent_channel).strip()
            if not canonical_channel:
                return []

            canonical_key = canonical_channel.casefold()
            assignments_by_source: dict[str, tuple[str, str]] = {}
            contributors = sorted(
                (
                    candidate
                    for candidate in loaded_channels
                    if resolve_channel_name(candidate).casefold() == canonical_key
                ),
                key=lambda value: (0 if value.casefold() == canonical_key else 1, value.casefold()),
            )

            for contributor in contributors:
                for source_name in channel_source_names(contributor, aggregate=False):
                    source_key = source_name.casefold()
                    if source_key in assignments_by_source:
                        continue
                    assignments_by_source[source_key] = (contributor, source_name)

            if not assignments_by_source:
                for source_name in channel_source_names(canonical_channel, aggregate=True):
                    source_key = source_name.casefold()
                    if source_key in assignments_by_source:
                        continue
                    assignments_by_source[source_key] = (canonical_channel, source_name)

            if not assignments_by_source:
                return [(canonical_channel, "Unknown Source")]

            ordered_source_keys = sorted(
                assignments_by_source.keys(),
                key=lambda source_key: (
                    source_priority_by_name.get(source_key, 10_000),
                    assignments_by_source[source_key][1].casefold(),
                ),
            )
            return [assignments_by_source[source_key] for source_key in ordered_source_keys]

        def refresh_channel_number_tree(values: list[str]) -> None:
            channel_number_item_channel.clear()
            for item_id in channel_number_tree.get_children():
                channel_number_tree.delete(item_id)

            parent_channels = sorted(values, key=lambda value: value.casefold())
            for number, parent_channel in enumerate(parent_channels, start=1):
                parent_id = f"channel_parent_{number}"
                channel_number_tree.insert(
                    "",
                    tk.END,
                    iid=parent_id,
                    text=format_channel_label(parent_channel, aggregate=True),
                    values=(str(number),),
                )
                channel_number_item_channel[parent_id] = parent_channel

                source_assignments = channel_source_assignments(parent_channel)
                for child_index, (child_channel, source_name) in enumerate(source_assignments, start=1):
                    child_id = f"{parent_id}_source_{child_index}"
                    source_display = source_name.strip() or "Unknown Source"
                    child_label = f"{child_channel} ({source_display})"
                    channel_number_tree.insert(
                        parent_id,
                        tk.END,
                        iid=child_id,
                        text=f"|- {child_label}",
                        values=(f"{number}.{child_index}",),
                    )
                    channel_number_item_channel[child_id] = child_channel

                if source_assignments:
                    channel_number_tree.item(parent_id, open=True)

        def visible_channels_from_loaded() -> list[str]:
            deduped: dict[str, str] = {}
            for channel in loaded_channels:
                canonical = resolve_channel_name(channel)
                key = canonical.casefold()
                if key not in deduped:
                    deduped[key] = canonical
            return sorted(deduped.values(), key=lambda value: value.casefold())

        def refresh_channel_views() -> None:
            normalize_merge_map_for_loaded_channels()
            visible_channels = visible_channels_from_loaded()

            visible_by_key = {channel.casefold(): channel for channel in visible_channels}
            excluded_keys: set[str] = set()
            normalized_excluded: list[str] = []
            for channel in exclude_channels:
                key = channel.casefold()
                normalized = visible_by_key.get(key)
                if normalized is None:
                    continue
                normalized_key = normalized.casefold()
                if normalized_key in excluded_keys:
                    continue
                excluded_keys.add(normalized_key)
                normalized_excluded.append(normalized)

            exclude_channels[:] = sorted(normalized_excluded, key=lambda value: value.casefold())
            include_channels[:] = [channel for channel in visible_channels if channel.casefold() not in excluded_keys]

            refresh_channel_listbox(include_listbox, include_channels, include_listbox_channels)
            refresh_channel_listbox(exclude_listbox, exclude_channels, exclude_listbox_channels)
            refresh_channel_number_tree(visible_channels)
            refresh_transfer_state()

        def refresh_for_source_reorder() -> None:
            if not popup.winfo_exists():
                return
            rebuild_source_priority_map()
            if loaded_channels:
                refresh_channel_views()
                channel_status_var.set(
                    "M3U source order changed. Channel source numbering was reassigned (.1, .2, ...)."
                )

        self.channel_source_reorder_callback = refresh_for_source_reorder

        def clear_source_reorder_callback(event: object | None = None) -> None:
            widget = getattr(event, "widget", None) if event is not None else popup
            if widget is not popup:
                return
            if self.channel_source_reorder_callback is refresh_for_source_reorder:
                self.channel_source_reorder_callback = None

        popup.bind("<Destroy>", clear_source_reorder_callback, add="+")

        def merge_channel_pair(source_channel: str, target_channel: str) -> bool:
            source_canonical = resolve_channel_name(source_channel.strip())
            target_canonical = resolve_channel_name(target_channel.strip())
            if not source_canonical or not target_canonical:
                return False
            if source_canonical.casefold() == target_canonical.casefold():
                return False

            source_key = source_canonical.casefold()
            target_key = target_canonical.casefold()
            source_was_excluded = any(channel.casefold() == source_key for channel in exclude_channels)
            target_is_excluded = any(channel.casefold() == target_key for channel in exclude_channels)

            for existing_source_key, existing_target in list(merge_map.items()):
                if existing_target.casefold() == source_key:
                    merge_map[existing_source_key] = target_canonical

            merge_map[source_key] = target_canonical
            merge_map.pop(target_key, None)
            if source_was_excluded and not target_is_excluded:
                exclude_channels.append(target_canonical)
            exclude_channels[:] = [channel for channel in exclude_channels if channel.casefold() != source_key]

            normalize_merge_map_for_loaded_channels()
            refresh_channel_views()
            channel_status_var.set(f"Merged '{source_canonical}' into '{target_canonical}'.")
            self._append_log(f"[APP] Merged channel '{source_canonical}' into '{target_canonical}'.")
            return True

        def open_merge_picker(base_channel: str) -> None:
            if not loaded_channels:
                self._show_error("Merge Channels", "No channels are loaded yet.")
                return

            base_canonical = resolve_channel_name(base_channel.strip())
            if not base_canonical:
                return

            base_key = base_canonical.casefold()
            candidates: list[str] = []
            for channel in sorted(loaded_channels, key=lambda value: value.casefold()):
                candidate_canonical = resolve_channel_name(channel)
                if candidate_canonical.casefold() == base_key:
                    continue
                candidates.append(channel)

            deduped_candidates: list[str] = []
            seen_candidate_keys: set[str] = set()
            for channel in candidates:
                key = channel.casefold()
                if key in seen_candidate_keys:
                    continue
                seen_candidate_keys.add(key)
                deduped_candidates.append(channel)

            if not deduped_candidates:
                messagebox.showinfo("Merge Channels", f"No other channels are available to merge into '{base_canonical}'.")
                return

            merge_popup = tk.Toplevel(popup)
            merge_popup.title("Merge Channels")
            merge_popup.geometry("520x420")
            merge_popup.minsize(460, 340)
            merge_popup.transient(popup)
            merge_popup.grab_set()

            frame = ttk.Frame(merge_popup, padding=12)
            frame.pack(fill=tk.BOTH, expand=True)
            frame.columnconfigure(0, weight=1)
            frame.rowconfigure(1, weight=1)

            ttk.Label(
                frame,
                text=(
                    f"Merge into: {format_channel_label(base_canonical, aggregate=True)}\n"
                    "Double-click a channel below to merge it into the selected channel."
                ),
            ).grid(row=0, column=0, sticky="w")

            candidate_list = tk.Listbox(frame, exportselection=False)
            candidate_list.grid(row=1, column=0, sticky="nsew", pady=(8, 0))
            candidate_scroll = ttk.Scrollbar(frame, orient=tk.VERTICAL, command=candidate_list.yview)
            candidate_scroll.grid(row=1, column=1, sticky="ns", pady=(8, 0))
            candidate_list.configure(yscrollcommand=candidate_scroll.set)

            candidate_channels: list[str] = list(deduped_candidates)
            for channel in candidate_channels:
                candidate_list.insert(tk.END, format_channel_label(channel, aggregate=False))

            footer_row = ttk.Frame(frame)
            footer_row.grid(row=2, column=0, columnspan=2, sticky="e", pady=(10, 0))

            def commit_merge(_event: object | None = None) -> None:
                selected = candidate_list.curselection()
                if not selected:
                    return
                index = selected[0]
                if not (0 <= index < len(candidate_channels)):
                    return
                selected_channel = candidate_channels[index]
                if not selected_channel:
                    return

                if merge_channel_pair(selected_channel, base_canonical):
                    merge_popup.destroy()

            ttk.Button(footer_row, text="Cancel", command=merge_popup.destroy).pack(side=tk.LEFT, padx=(0, 8))
            ttk.Button(footer_row, text="Merge", command=commit_merge).pack(side=tk.LEFT)

            candidate_list.bind("<Double-Button-1>", commit_merge)
            candidate_list.focus_set()

        def open_merge_from_context() -> None:
            base_channel = context_channel.get("name", "").strip()
            if not base_channel:
                return
            open_merge_picker(base_channel)

        def unmerge_candidates_for_channel(channel: str) -> list[str]:
            channel_name = channel.strip()
            if not channel_name:
                return []

            channel_name_by_key = {item.casefold(): item for item in loaded_channels}
            selected_key = channel_name.casefold()
            selected_canonical_key = resolve_channel_name(channel_name).casefold()
            candidates: dict[str, str] = {}

            if selected_key in merge_map:
                candidates[selected_key] = channel_name_by_key.get(selected_key, channel_name)

            for source_key in merge_map.keys():
                source_name = channel_name_by_key.get(source_key, source_key)
                source_canonical_key = resolve_channel_name(source_name).casefold()
                if source_canonical_key == selected_canonical_key:
                    candidates[source_key] = source_name

            return sorted(candidates.values(), key=lambda value: value.casefold())

        def all_merged_source_channels() -> list[str]:
            channel_name_by_key = {item.casefold(): item for item in loaded_channels}
            merged_sources = [channel_name_by_key.get(source_key, source_key) for source_key in merge_map.keys()]
            deduped: dict[str, str] = {}
            for source_name in merged_sources:
                key = str(source_name).strip().casefold()
                if not key:
                    continue
                if key not in deduped:
                    deduped[key] = str(source_name).strip()
            return sorted(deduped.values(), key=lambda value: value.casefold())

        def unmerge_channel(channel: str) -> bool:
            channel_name = channel.strip()
            if not channel_name:
                return False

            channel_key = channel_name.casefold()
            if channel_key not in merge_map:
                return False

            target_name = str(merge_map.get(channel_key, "")).strip()
            merge_map.pop(channel_key, None)
            normalize_merge_map_for_loaded_channels()
            refresh_channel_views()
            channel_status_var.set(f"Un-merged '{channel_name}' from '{target_name}'.")
            self._append_log(f"[APP] Un-merged channel '{channel_name}' from '{target_name}'.")
            return True

        def open_unmerge_picker(channels: list[str], selected_context: str) -> None:
            candidate_channels = [ch for ch in channels if str(ch).strip()]
            if not candidate_channels:
                return

            picker = tk.Toplevel(popup)
            picker.title("Un-merge Channels")
            picker.geometry("520x420")
            picker.minsize(460, 320)
            picker.transient(popup)
            picker.grab_set()

            frame = ttk.Frame(picker, padding=12)
            frame.pack(fill=tk.BOTH, expand=True)
            frame.columnconfigure(0, weight=1)
            frame.rowconfigure(1, weight=1)

            ttk.Label(
                frame,
                text=(
                    f"Selected: {selected_context}\n"
                    "Choose merged channel(s) to un-merge."
                ),
            ).grid(row=0, column=0, sticky="w")

            candidates_list = tk.Listbox(frame, exportselection=False, selectmode=tk.EXTENDED)
            candidates_list.grid(row=1, column=0, sticky="nsew", pady=(8, 0))
            candidates_scroll = ttk.Scrollbar(frame, orient=tk.VERTICAL, command=candidates_list.yview)
            candidates_scroll.grid(row=1, column=1, sticky="ns", pady=(8, 0))
            candidates_list.configure(yscrollcommand=candidates_scroll.set)

            for channel_name in candidate_channels:
                candidates_list.insert(tk.END, format_channel_label(channel_name, aggregate=False))

            actions = ttk.Frame(frame)
            actions.grid(row=2, column=0, columnspan=2, sticky="e", pady=(10, 0))

            def commit_selected(_event: object | None = None) -> None:
                selected_indices = list(candidates_list.curselection())
                if not selected_indices:
                    return

                unmerged = 0
                for idx in selected_indices:
                    if 0 <= idx < len(candidate_channels):
                        if unmerge_channel(candidate_channels[idx]):
                            unmerged += 1

                if unmerged > 0:
                    picker.destroy()
                else:
                    messagebox.showinfo("Un-merge Channels", "No selected channels were un-merged.", parent=picker)

            def commit_all() -> None:
                unmerged = 0
                for channel_name in candidate_channels:
                    if unmerge_channel(channel_name):
                        unmerged += 1
                if unmerged > 0:
                    picker.destroy()
                else:
                    messagebox.showinfo("Un-merge Channels", "No channels were un-merged.", parent=picker)

            ttk.Button(actions, text="Cancel", command=picker.destroy).pack(side=tk.LEFT, padx=(0, 8))
            ttk.Button(actions, text="Un-merge All", command=commit_all).pack(side=tk.LEFT, padx=(0, 8))
            ttk.Button(actions, text="Un-merge Selected", command=commit_selected).pack(side=tk.LEFT)
            candidates_list.bind("<Double-Button-1>", commit_selected)
            candidates_list.focus_set()

        def open_unmerge_from_context() -> None:
            channel_name = context_channel.get("name", "").strip()
            if not channel_name:
                return
            candidates = unmerge_candidates_for_channel(channel_name)
            if not candidates:
                candidates = all_merged_source_channels()

            if len(candidates) == 1:
                if not unmerge_channel(candidates[0]):
                    messagebox.showinfo("Un-merge Channel", f"'{candidates[0]}' is not currently merged.", parent=popup)
                return

            if not candidates:
                messagebox.showinfo("Un-merge Channel", f"'{channel_name}' is not currently merged.", parent=popup)
                return

            open_unmerge_picker(candidates, channel_name)

        context_menu.add_command(label="Merge", command=open_merge_from_context)
        context_menu.add_command(label="Un-merge", command=open_unmerge_from_context)

        def show_channel_context_menu(channel: str, x_root: int, y_root: int) -> None:
            channel_name = channel.strip()
            if not channel_name:
                return
            context_channel["name"] = channel_name
            if unmerge_candidates_for_channel(channel_name) or bool(merge_map):
                context_menu.entryconfigure("Un-merge", state="normal")
                self._set_last_event(f"Merged channel selected: {channel_name}")
            else:
                context_menu.entryconfigure("Un-merge", state="disabled")
                self._set_last_event(f"Channel selected for merge: {channel_name}")
            try:
                context_menu.tk_popup(x_root, y_root)
            finally:
                context_menu.grab_release()

        def _event_int(value: object, default: int = 0) -> int:
            try:
                return int(value)  # type: ignore[arg-type]
            except (TypeError, ValueError):
                return default

        def on_include_right_click(event: object) -> str:
            index = include_listbox.nearest(_event_int(getattr(event, "y", -1), -1))
            if index < 0 or index >= include_listbox.size():
                return "break"
            include_listbox.selection_clear(0, tk.END)
            include_listbox.selection_set(index)
            if not (0 <= index < len(include_listbox_channels)):
                return "break"
            channel = include_listbox_channels[index]
            x_root = _event_int(getattr(event, "x_root", popup.winfo_pointerx()), popup.winfo_pointerx())
            y_root = _event_int(getattr(event, "y_root", popup.winfo_pointery()), popup.winfo_pointery())
            show_channel_context_menu(channel, x_root, y_root)
            return "break"

        def on_exclude_right_click(event: object) -> str:
            index = exclude_listbox.nearest(_event_int(getattr(event, "y", -1), -1))
            if index < 0 or index >= exclude_listbox.size():
                return "break"
            exclude_listbox.selection_clear(0, tk.END)
            exclude_listbox.selection_set(index)
            if not (0 <= index < len(exclude_listbox_channels)):
                return "break"
            channel = exclude_listbox_channels[index]
            x_root = _event_int(getattr(event, "x_root", popup.winfo_pointerx()), popup.winfo_pointerx())
            y_root = _event_int(getattr(event, "y_root", popup.winfo_pointery()), popup.winfo_pointery())
            show_channel_context_menu(channel, x_root, y_root)
            return "break"

        def on_number_tree_right_click(event: object) -> str:
            row_id = channel_number_tree.identify_row(_event_int(getattr(event, "y", -1), -1))
            if not row_id:
                return "break"
            channel_number_tree.selection_set(row_id)
            channel = channel_number_item_channel.get(row_id, "").strip()
            if not channel:
                return "break"
            x_root = _event_int(getattr(event, "x_root", popup.winfo_pointerx()), popup.winfo_pointerx())
            y_root = _event_int(getattr(event, "y_root", popup.winfo_pointery()), popup.winfo_pointery())
            show_channel_context_menu(channel, x_root, y_root)
            return "break"

        def _channel_from_include_event(event: object) -> str:
            index = include_listbox.nearest(_event_int(getattr(event, "y", -1), -1))
            if index < 0 or index >= include_listbox.size():
                return ""
            if not (0 <= index < len(include_listbox_channels)):
                return ""
            return include_listbox_channels[index]

        def _channel_from_exclude_event(event: object) -> str:
            index = exclude_listbox.nearest(_event_int(getattr(event, "y", -1), -1))
            if index < 0 or index >= exclude_listbox.size():
                return ""
            if not (0 <= index < len(exclude_listbox_channels)):
                return ""
            return exclude_listbox_channels[index]

        def _channel_from_tree_event(event: object) -> str:
            row_id = channel_number_tree.identify_row(_event_int(getattr(event, "y", -1), -1))
            if not row_id:
                return ""
            return channel_number_item_channel.get(row_id, "").strip()

        def _start_drag_from_channel(channel: str, event: object) -> None:
            drag_state["channel"] = channel
            drag_state["x_root"] = _event_int(getattr(event, "x_root", 0), 0)
            drag_state["y_root"] = _event_int(getattr(event, "y_root", 0), 0)

        def _complete_drag_on_target(target_channel: str) -> None:
            source_channel = str(drag_state.get("channel", "")).strip()
            drag_state["channel"] = ""
            if not source_channel or not target_channel:
                return

            source_canonical = resolve_channel_name(source_channel)
            target_canonical = resolve_channel_name(target_channel)
            if not source_canonical or not target_canonical:
                return
            if source_canonical.casefold() == target_canonical.casefold():
                return

            should_merge = messagebox.askyesno(
                "Merge channels?",
                f"Merge '{source_canonical}' into '{target_canonical}'?",
                parent=popup,
            )
            if not should_merge:
                return

            merge_channel_pair(source_canonical, target_canonical)

        def on_include_drag_start(event: object) -> str:
            channel = _channel_from_include_event(event)
            if not channel:
                return "break"
            index = include_listbox.nearest(_event_int(getattr(event, "y", -1), -1))
            include_listbox.selection_clear(0, tk.END)
            include_listbox.selection_set(index)
            _start_drag_from_channel(channel, event)
            return "break"

        def on_exclude_drag_start(event: object) -> str:
            channel = _channel_from_exclude_event(event)
            if not channel:
                return "break"
            index = exclude_listbox.nearest(_event_int(getattr(event, "y", -1), -1))
            exclude_listbox.selection_clear(0, tk.END)
            exclude_listbox.selection_set(index)
            _start_drag_from_channel(channel, event)
            return "break"

        def on_tree_drag_start(event: object) -> str:
            channel = _channel_from_tree_event(event)
            if not channel:
                return "break"
            row_id = channel_number_tree.identify_row(_event_int(getattr(event, "y", -1), -1))
            if row_id:
                channel_number_tree.selection_set(row_id)
            _start_drag_from_channel(channel, event)
            return "break"

        def on_include_drag_release(event: object) -> str:
            _complete_drag_on_target(_channel_from_include_event(event))
            return "break"

        def on_exclude_drag_release(event: object) -> str:
            _complete_drag_on_target(_channel_from_exclude_event(event))
            return "break"

        def on_tree_drag_release(event: object) -> str:
            _complete_drag_on_target(_channel_from_tree_event(event))
            return "break"

        def transfer_selected_channels() -> None:
            selected_from_include = selected_channels_from_listbox(include_listbox, include_listbox_channels)
            selected_from_exclude = selected_channels_from_listbox(exclude_listbox, exclude_listbox_channels)
            if not selected_from_include and not selected_from_exclude:
                return

            for channel in selected_from_include:
                include_channels[:] = [item for item in include_channels if item != channel]
                if channel not in exclude_channels:
                    exclude_channels.append(channel)

            for channel in selected_from_exclude:
                exclude_channels[:] = [item for item in exclude_channels if item != channel]
                if channel not in include_channels:
                    include_channels.append(channel)

            refresh_channel_listbox(include_listbox, include_channels, include_listbox_channels)
            refresh_channel_listbox(exclude_listbox, exclude_channels, exclude_listbox_channels)

        def current_channel_selection() -> str:
            include_sel = include_listbox.curselection()
            if include_sel:
                index = include_sel[0]
                if 0 <= index < len(include_listbox_channels):
                    return include_listbox_channels[index]

            exclude_sel = exclude_listbox.curselection()
            if exclude_sel:
                index = exclude_sel[0]
                if 0 <= index < len(exclude_listbox_channels):
                    return exclude_listbox_channels[index]

            tree_sel = channel_number_tree.selection()
            if tree_sel:
                return channel_number_item_channel.get(tree_sel[0], "").strip()

            return ""

        def merge_selected_channel() -> None:
            channel = current_channel_selection()
            if not channel:
                self._show_error(
                    "Merge Channels",
                    "Select a channel first, then right-click it and choose Merge, or use Merge Selected.",
                )
                return
            open_merge_picker(channel)

        def save_popup_settings() -> None:
            include_patterns: list[str] = []
            exclude_patterns = [
                self._channel_name_to_exact_pattern(channel)
                for channel in sorted(exclude_channels, key=lambda v: v.casefold())
            ]

            self.include_title_filters = include_patterns + include_unmapped_filters
            self.exclude_title_filters = exclude_patterns + exclude_unmapped_filters
            # Source assignment in this dialog is now automatic, so clear manual channel-source rules.
            self.channel_source_rules = []
            loaded_keys = {channel.casefold() for channel in loaded_channels}
            merge_rules: list[dict[str, str]] = []
            channel_name_by_key = {channel.casefold(): channel for channel in loaded_channels}
            for source_key in sorted(merge_map.keys(), key=lambda value: channel_name_by_key.get(value, value).casefold()):
                source_name = channel_name_by_key.get(source_key, source_key)
                target_name = str(merge_map.get(source_key, "")).strip()
                if not source_name or not target_name:
                    continue
                if source_name.casefold() == target_name.casefold():
                    continue
                merge_rules.append({"source": source_name, "target": target_name})

            preserved_merge_rules: list[dict[str, str]] = []
            for rule in self.channel_merge_rules:
                source = str(rule.get("source", "")).strip()
                target = str(rule.get("target", "")).strip()
                if not source or not target:
                    continue
                if source.casefold() == target.casefold():
                    continue
                if source.casefold() in loaded_keys:
                    continue
                preserved_merge_rules.append({"source": source, "target": target})

            self.channel_merge_rules = self._normalize_channel_merge_rules(merge_rules + preserved_merge_rules)

            try:
                settings = self._collect_settings()
                self._persist_settings(settings, sync_start_on_boot=True, show_dialogs=False, log_event=False)
            except Exception as exc:  # noqa: BLE001
                self._show_error("Invalid Settings", str(exc))
                return

            self._append_log("[APP] Channel settings saved.")
            self._restart_server_if_running("Channel settings changed.")
            popup.destroy()

        def refresh_channels() -> None:
            source_snapshot = [dict(source) for source in self.sources_items if str(source.get("url", "")).strip()]
            if not source_snapshot:
                loaded_channels.clear()
                channel_status_var.set("No sources configured. Add sources first.")
                include_channels.clear()
                exclude_channels.clear()
                refresh_channel_views()
                return

            refresh_channels_button.state(["disabled"])
            channel_status_var.set("Loading channels from all configured sources...")

            def worker() -> None:
                channels, channel_sources, errors = self._discover_channels_from_sources(source_snapshot)

                def done() -> None:
                    if not popup.winfo_exists():
                        return

                    refresh_channels_button.state(["!disabled"])
                    if channels:
                        loaded_channels[:] = list(channels)
                        loaded_channel_sources.clear()
                        loaded_channel_sources.update(channel_sources)
                        refresh_channel_views()
                        visible_count = len(visible_channels_from_loaded())
                        channel_status_var.set(
                            f"{len(channels)} channel(s) loaded. {visible_count} visible after merges."
                        )
                    else:
                        loaded_channels.clear()
                        loaded_channel_sources.clear()
                        channel_status_var.set("No channels found. Check source URLs and click Refresh.")
                        include_channels.clear()
                        exclude_channels.clear()
                        refresh_channel_views()

                    if errors:
                        for error in errors[:3]:
                            self._append_log(f"[APP] {error}")
                        if len(errors) > 3:
                            self._append_log(f"[APP] Channel discovery skipped {len(errors) - 3} additional source(s).")

                self.after(0, done)

            threading.Thread(target=worker, daemon=True).start()

        transfer_button.configure(command=transfer_selected_channels)
        merge_selected_button.configure(command=merge_selected_channel)
        refresh_channels_button.configure(command=refresh_channels)
        save_button.configure(command=save_popup_settings)
        include_listbox.bind("<ButtonPress-1>", on_include_drag_start)
        include_listbox.bind("<ButtonRelease-1>", on_include_drag_release)
        include_listbox.bind("<Button-3>", on_include_right_click)
        include_listbox.bind("<ButtonRelease-3>", on_include_right_click)
        include_listbox.bind("<Button-2>", on_include_right_click)
        include_listbox.bind("<ButtonRelease-2>", on_include_right_click)
        exclude_listbox.bind("<ButtonPress-1>", on_exclude_drag_start)
        exclude_listbox.bind("<ButtonRelease-1>", on_exclude_drag_release)
        exclude_listbox.bind("<Button-3>", on_exclude_right_click)
        exclude_listbox.bind("<ButtonRelease-3>", on_exclude_right_click)
        exclude_listbox.bind("<Button-2>", on_exclude_right_click)
        exclude_listbox.bind("<ButtonRelease-2>", on_exclude_right_click)
        channel_number_tree.bind("<ButtonPress-1>", on_tree_drag_start)
        channel_number_tree.bind("<ButtonRelease-1>", on_tree_drag_release)
        channel_number_tree.bind("<Button-3>", on_number_tree_right_click)
        channel_number_tree.bind("<ButtonRelease-3>", on_number_tree_right_click)
        channel_number_tree.bind("<Button-2>", on_number_tree_right_click)
        channel_number_tree.bind("<ButtonRelease-2>", on_number_tree_right_click)

        refresh_channel_listbox(include_listbox, include_channels, include_listbox_channels)
        refresh_channel_listbox(exclude_listbox, exclude_channels, exclude_listbox_channels)
        refresh_channel_number_tree(loaded_channels)
        refresh_transfer_state()

        if include_unmapped_filters or exclude_unmapped_filters:
            channel_status_var.set(
                "Channel filters loaded. Existing advanced regex rules are preserved in the background."
            )

        refresh_channels()

    def _collect_settings(self) -> dict:
        port = self.port_var.get().strip()
        if not port.isdigit():
            raise ValueError("Port must be a valid integer.")

        max_retries = self.max_retries_var.get().strip() or "5"
        retry_wait = self.retry_wait_var.get().strip() or "0"
        stream_timeout = self.stream_timeout_var.get().strip() or "7"
        if not max_retries.isdigit() or int(max_retries) < 0:
            raise ValueError("Max retries must be 0 or greater.")
        if not retry_wait.isdigit() or int(retry_wait) < 0:
            raise ValueError("Retry wait must be 0 or greater.")
        if not stream_timeout.isdigit() or int(stream_timeout) <= 0:
            raise ValueError("Stream timeout must be greater than 0.")

        sources: list[dict[str, str]] = []
        for index, source in enumerate(self.sources_items, start=1):
            url = str(source.get("url", "")).strip()
            if not url:
                continue

            name = str(source.get("name", "")).strip() or f"Source {index}"
            concurrency = str(source.get("concurrency", "1")).strip() or "1"
            if not concurrency.isdigit() or int(concurrency) < 1:
                raise ValueError(f"Invalid concurrency for source '{name}'.")

            sources.append(
                {
                    "name": name,
                    "url": url,
                    "concurrency": concurrency,
                }
            )

        return {
            "port": port,
            "base_url": self.base_url_var.get().strip(),
            "timezone": self.timezone_var.get().strip(),
            "sync_cron": self.sync_cron_var.get().strip() or "0 0 * * *",
            "start_on_boot": bool(self.start_on_boot_var.get()),
            "sync_on_boot": bool(self.sync_on_boot_var.get()),
            "clear_on_boot": bool(self.clear_on_boot_var.get()),
            "credentials": self.credentials_var.get().strip(),
            "max_retries": max_retries,
            "retry_wait": retry_wait,
            "stream_timeout": stream_timeout,
            "sources": sources,
            "include_title_filters": list(self.include_title_filters),
            "exclude_title_filters": list(self.exclude_title_filters),
            "channel_source_rules": list(self.channel_source_rules),
            "channel_merge_rules": list(self.channel_merge_rules),
            "web_discovery_jobs": [dict(job) for job in self.web_discovery_jobs],
        }

    def _persist_settings(
        self,
        settings: dict,
        sync_start_on_boot: bool = False,
        show_dialogs: bool = False,
        log_event: bool = False,
    ) -> None:
        SETTINGS_FILE.write_text(json.dumps(settings, indent=2), encoding="utf-8")
        if sync_start_on_boot:
            self._sync_start_on_boot(
                bool(settings.get("start_on_boot", False)),
                show_dialogs=show_dialogs,
                log_event=log_event,
            )

    def _save_settings_clicked(self) -> None:
        try:
            settings = self._collect_settings()
            self._persist_settings(settings, sync_start_on_boot=True, show_dialogs=True, log_event=True)
            self._append_log("[APP] Settings saved.")
        except Exception as exc:  # noqa: BLE001
            self._show_error("Invalid Settings", str(exc))

    def _export_backup_clicked(self) -> None:
        try:
            settings = self._collect_settings()
        except Exception as exc:  # noqa: BLE001
            self._show_error("Export Backup", f"Cannot export while settings are invalid:\n{exc}")
            return

        timestamp = time.strftime("%Y%m%d-%H%M%S")
        backup_file = filedialog.asksaveasfilename(
            parent=self,
            title="Export Settings Backup",
            defaultextension=".json",
            initialfile=f"windows-m3u-stream-merger-proxy-backup-{timestamp}.json",
            filetypes=[("JSON Files", "*.json"), ("All Files", "*.*")],
        )
        if not backup_file:
            return

        backup_path = Path(backup_file)
        try:
            backup_path.write_text(json.dumps(settings, indent=2), encoding="utf-8")
        except OSError as exc:
            self._show_error("Export Backup", f"Could not write backup file:\n{exc}")
            return

        self._append_log(f"[APP] Backup exported: {backup_path}")
        self._set_last_event(f"Backup exported: {backup_path}")

    def _load_backup_clicked(self) -> None:
        backup_file = filedialog.askopenfilename(
            parent=self,
            title="Load Settings Backup",
            filetypes=[("JSON Files", "*.json"), ("All Files", "*.*")],
        )
        if not backup_file:
            return

        backup_path = Path(backup_file)
        try:
            loaded = json.loads(backup_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            self._show_error("Load Backup", f"Could not read backup file:\n{exc}")
            return

        if not isinstance(loaded, dict):
            self._show_error("Load Backup", "Backup file must contain a JSON object.")
            return

        settings = self._merge_with_default_settings(loaded)
        self._apply_settings_to_ui(settings)

        try:
            persisted = self._collect_settings()
            self._persist_settings(persisted, sync_start_on_boot=True, show_dialogs=True, log_event=True)
        except Exception as exc:  # noqa: BLE001
            self._show_error("Load Backup", f"Backup contains invalid values:\n{exc}")
            return

        self._append_log(f"[APP] Backup loaded: {backup_path}")
        self._set_last_event(f"Backup loaded: {backup_path}")

    def _open_data_folder_clicked(self) -> None:
        APP_STATE_DIR.mkdir(parents=True, exist_ok=True)
        try:
            os.startfile(str(APP_STATE_DIR))
        except OSError as exc:
            self._show_error("Error", f"Could not open folder:\n{exc}")

    def _open_playlist_clicked(self) -> None:
        playlist_url = self._current_base_url().rstrip("/") + "/playlist.m3u"
        webbrowser.open(playlist_url)

    def _is_local_only_base_url(self, configured_base_url: str) -> bool:
        candidate = str(configured_base_url).strip()
        if not candidate:
            return False

        if "://" not in candidate:
            candidate = f"http://{candidate}"

        try:
            parsed = urllib.parse.urlsplit(candidate)
        except Exception:  # noqa: BLE001
            return False

        return _is_local_only_host(parsed.hostname or "")

    def _effective_base_url(self, configured_base_url: str, port: str) -> str:
        configured = str(configured_base_url).strip()
        clean_port = str(port).strip() or "8080"
        if configured:
            candidate = configured
            if "://" not in candidate:
                candidate = f"http://{candidate}"

            try:
                parsed = urllib.parse.urlsplit(candidate)
                if _is_local_only_host(parsed.hostname or ""):
                    lan_ip = _detect_lan_ipv4()
                    # If LAN detection fails, keep the configured value.
                    if lan_ip and not lan_ip.startswith("127."):
                        scheme = parsed.scheme or "http"
                        effective_port = parsed.port or clean_port
                        netloc = f"{lan_ip}:{effective_port}" if effective_port else lan_ip
                        rebuilt = urllib.parse.urlunsplit((scheme, netloc, parsed.path, parsed.query, parsed.fragment))
                        return rebuilt.rstrip("/")

                return candidate.rstrip("/")
            except Exception:  # noqa: BLE001
                return configured.rstrip("/")

        lan_ip = _detect_lan_ipv4()
        return f"http://{lan_ip}:{clean_port}"

    def _current_base_url(self) -> str:
        port = self.port_var.get().strip() or "8080"
        configured = self.base_url_var.get().strip()
        return self._effective_base_url(configured, port)

    def _set_last_event(self, message: str) -> None:
        self.last_event_var.set(message)

    def _set_status_state(self, state: str, detail: str) -> None:
        self.status_var.set(state)
        self.status_detail_var.set(detail)
        if state == "RUNNING":
            self.status_state_entry.configure(fg="#137333")
        elif state == "ERROR":
            self.status_state_entry.configure(fg="#b42318")
        else:
            self.status_state_entry.configure(fg="#444444")

    def _update_status_display(self) -> None:
        playlist_url = f"{self._current_base_url()}/playlist.m3u"
        self.playlist_url_var.set(playlist_url)

        proc = self.server_process
        if proc and proc.poll() is None:
            self._set_status_state("RUNNING", f"Server process active on port {self.port_var.get().strip() or '8080'}.")
            return

        if self.last_exit_code is not None:
            if self.stop_requested_by_user:
                detail = "Server stopped by user."
                if self.last_exit_code != 0:
                    detail = f"Server stopped by user (exit code: {self.last_exit_code})."
                self._set_status_state("STOPPED", detail)
            elif self.last_exit_code == 0:
                self._set_status_state("STOPPED", "Server process stopped cleanly.")
            else:
                detail = f"Server process exited (code: {self.last_exit_code})."
                if self.last_server_error:
                    detail = f"{detail} {self.last_server_error}"
                elif self.last_server_line:
                    detail = f"{detail} Last log: {self.last_server_line}"
                self._set_status_state("ERROR", detail)
            return

        self._set_status_state("STOPPED", "Server process is not running.")

    def _create_tray_image(self) -> Image.Image:
        size = 64
        image = Image.new("RGBA", (size, size), (20, 24, 28, 255))
        draw = ImageDraw.Draw(image)
        draw.rounded_rectangle((8, 8, 56, 56), radius=10, fill=(40, 133, 255, 255))
        draw.ellipse((20, 20, 44, 44), fill=(255, 255, 255, 255))
        return image

    def _ensure_tray_icon(self) -> None:
        if self.tray_icon is not None:
            return

        menu = pystray.Menu(
            pystray.MenuItem("Open", self._tray_open_clicked, default=True),
            pystray.MenuItem("Exit", self._tray_exit_clicked),
        )
        self.tray_icon = pystray.Icon("m3u_proxy_desktop", self._create_tray_image(), APP_NAME, menu)

    def _show_tray(self, notify_message: str) -> bool:
        def ensure_visible() -> bool:
            self._ensure_tray_icon()
            if self.tray_icon is None:
                return False
            if not self.tray_icon.visible:
                self.tray_icon.run_detached()
                deadline = time.time() + 2.0
                while time.time() < deadline:
                    try:
                        if self.tray_icon is not None and self.tray_icon.visible:
                            break
                    except Exception:  # noqa: BLE001
                        break
                    time.sleep(0.05)
            if self.tray_icon is None:
                return False
            if not self.tray_icon.visible:
                return False
            return True

        try:
            if not ensure_visible():
                return False
        except Exception:  # noqa: BLE001
            # Recreate and retry once because some backends cannot restart a stopped icon instance.
            self.tray_icon = None
            try:
                if not ensure_visible():
                    return False
            except Exception:  # noqa: BLE001
                return False

        def notify_worker() -> None:
            # Retry a few times because tray backend can lag right after run_detached().
            for _ in range(4):
                try:
                    if self.tray_icon is not None and self.tray_icon.visible:
                        self.tray_icon.notify(notify_message, APP_NAME)
                        break
                except Exception:  # noqa: BLE001
                    pass
                time.sleep(0.35)

            # Show explicit confirmation in-app every close-to-tray action.
            self.after(
                0,
                lambda: messagebox.showinfo(
                    APP_NAME,
                    "Server still running.\n\nThe app is minimized to the system tray.\nRight-click the tray icon and select Exit to close completely.",
                ),
            )

        threading.Thread(target=notify_worker, daemon=True).start()
        return True

    def _hide_tray(self) -> None:
        if self.tray_icon is None:
            return
        try:
            if self.tray_icon.visible:
                self.tray_icon.stop()
        except Exception:  # noqa: BLE001
            pass
        finally:
            # Recreate icon object on next minimize to avoid stale backend state.
            self.tray_icon = None

    def _tray_open_clicked(self, icon: pystray.Icon, item: pystray.MenuItem) -> None:
        del icon, item
        self.after(0, self._restore_from_tray)

    def _tray_exit_clicked(self, icon: pystray.Icon, item: pystray.MenuItem) -> None:
        del icon, item
        self.after(0, self._exit_application_from_tray)

    def _restore_from_tray(self) -> None:
        self.deiconify()
        self.lift()
        self.focus_force()
        self._hide_tray()
        self._set_last_event("Window restored from tray.")

    def _exit_application_from_tray(self) -> None:
        self.is_quitting = True
        self._stop_server_clicked()
        extra_killed = self._terminate_managed_server_processes()
        if extra_killed > 0:
            self._append_log(f"[APP] Terminated {extra_killed} leftover server process(es).")
        self._hide_tray()
        self._set_last_event("Application exited from tray menu.")
        self._release_instance_lock()
        self.destroy()

    def _server_candidates(self) -> list[Path]:
        exe_dir = Path(sys.executable).resolve().parent
        meipass = getattr(sys, "_MEIPASS", None)

        search_roots: list[Path] = []
        if meipass:
            search_roots.append(Path(meipass))
        search_roots.extend(
            [
                _resource_root(),
                exe_dir,
                exe_dir / "_internal",
                Path(__file__).resolve().parent,
                Path.cwd(),
            ]
        )

        unique_roots: list[Path] = []
        seen: set[str] = set()
        for root in search_roots:
            key = str(root.resolve()).lower()
            if key in seen:
                continue
            seen.add(key)
            unique_roots.append(root)

        candidates: list[Path] = []
        for root in unique_roots:
            candidates.extend(
                [
                    root / "server" / "windows-m3u-stream-merger-proxy.exe",
                    root / "_internal" / "server" / "windows-m3u-stream-merger-proxy.exe",
                    root / "windows-m3u-stream-merger-proxy.exe",
                ]
            )
        return candidates

    def _is_managed_server_path(self, path: str) -> bool:
        if not path:
            return False
        normalized = str(Path(path).resolve()).lower()
        app_root = str(APP_STATE_DIR.resolve()).lower()
        if normalized.startswith(app_root):
            return True

        exe_dir = Path(sys.executable).resolve().parent
        packaged_prefix = str((exe_dir / "_internal" / "server").resolve()).lower()
        return normalized.startswith(packaged_prefix)

    def _try_recover_port_from_stale_server(self, port: int) -> bool:
        listeners = _get_listening_processes(port)
        if not listeners:
            return False

        stale_pids: list[int] = []
        for info in listeners:
            pid_str = str(info.get("Pid", "")).strip()
            name = str(info.get("Name", "")).strip().lower()
            path = str(info.get("Path", "")).strip()
            if not pid_str.isdigit():
                continue
            if name not in {"windows-m3u-stream-merger-proxy.exe", "windows-m3u-stream-merger-proxy"}:
                continue
            if not self._is_managed_server_path(path):
                continue
            stale_pids.append(int(pid_str))

        if not stale_pids:
            return False

        self._append_log(f"[APP] Found stale managed server on port {port}. Attempting recovery...")
        for pid in stale_pids:
            _terminate_pid(pid)

        deadline = time.time() + 5
        while time.time() < deadline:
            if _is_port_available(port):
                self._append_log("[APP] Recovered port from stale server process.")
                return True
            time.sleep(0.2)

        return _is_port_available(port)

    def _terminate_managed_server_processes(self) -> int:
        proc_infos = _get_processes_by_name(["windows-m3u-stream-merger-proxy.exe", "windows-m3u-stream-merger-proxy"])
        if not proc_infos:
            return 0

        current_pid = None
        if self.server_process is not None:
            current_pid = self.server_process.pid

        killed = 0
        for info in proc_infos:
            pid_str = str(info.get("Pid", "")).strip()
            path = str(info.get("Path", "")).strip()
            if not pid_str.isdigit():
                continue
            pid = int(pid_str)
            if current_pid is not None and pid == current_pid:
                # _stop_server_clicked already handles the attached child.
                continue
            if not self._is_managed_server_path(path):
                continue
            if _terminate_pid(pid):
                killed += 1

        if killed > 0:
            deadline = time.time() + 4
            while time.time() < deadline:
                still_running = 0
                for info in _get_processes_by_name(["windows-m3u-stream-merger-proxy.exe", "windows-m3u-stream-merger-proxy"]):
                    pid_str = str(info.get("Pid", "")).strip()
                    path = str(info.get("Path", "")).strip()
                    if pid_str.isdigit() and self._is_managed_server_path(path):
                        still_running += 1
                if still_running == 0:
                    break
                time.sleep(0.2)

        return killed

    def _ensure_runtime_server_binary(self) -> Path | None:
        candidates = self._server_candidates()
        source = next((path for path in candidates if path.exists()), None)
        if source is None:
            return None

        destination = RUNTIME_DIR / "windows-m3u-stream-merger-proxy.exe"
        try:
            source_stat = source.stat()
            should_copy = not destination.exists()

            if not should_copy:
                dest_stat = destination.stat()
                if source_stat.st_size != dest_stat.st_size:
                    should_copy = True
                else:
                    source_hash = self._file_sha256(source)
                    dest_hash = self._file_sha256(destination)
                    if source_hash and dest_hash:
                        should_copy = source_hash != dest_hash
                    else:
                        # If hashing fails, fall back to a conservative replacement.
                        should_copy = True

            if should_copy:
                destination.parent.mkdir(parents=True, exist_ok=True)
                temp_destination = destination.with_suffix(".tmp")
                shutil.copy2(source, temp_destination)
                os.replace(temp_destination, destination)
                self._append_log(f"[APP] Server binary refreshed: {destination}")
            return destination
        except OSError:
            # Fallback: run the bundled server directly if runtime copy is blocked.
            return source

        return destination

    def _file_sha256(self, file_path: Path) -> str | None:
        try:
            digest = hashlib.sha256()
            with file_path.open("rb") as handle:
                while True:
                    chunk = handle.read(1024 * 1024)
                    if not chunk:
                        break
                    digest.update(chunk)
            return digest.hexdigest()
        except OSError:
            return None

    def _start_server(self, show_dialogs: bool = True) -> bool:
        if self.server_process and self.server_process.poll() is None:
            if show_dialogs:
                messagebox.showinfo("Server Already Running", "The server is already running.")
            self._set_last_event("Server already running.")
            self._update_status_display()
            return True

        self.stop_requested_by_user = False

        try:
            settings = self._collect_settings()
            self._persist_settings(settings, sync_start_on_boot=True, show_dialogs=False, log_event=False)
        except Exception as exc:  # noqa: BLE001
            if show_dialogs:
                self._show_error("Invalid Settings", str(exc))
            else:
                self._append_log(f"[APP] Auto-start skipped: {exc}")
            self._set_status_state("ERROR", "Settings are invalid. Fix values and try again.")
            self._set_last_event(f"Invalid settings: {exc}")
            return False

        server_bin = self._ensure_runtime_server_binary()
        if server_bin is None:
            if show_dialogs:
                self._show_error(
                    "Missing Server Binary",
                    "windows-m3u-stream-merger-proxy.exe was not found.\n\nRun windows-app\\build.ps1 to build the bundled server and GUI.",
                )
            else:
                self._append_log("[APP] Auto-start failed: missing windows-m3u-stream-merger-proxy.exe.")
            self._set_status_state("ERROR", "Bundled server binary not found.")
            self._set_last_event("Start failed: missing windows-m3u-stream-merger-proxy.exe.")
            return False

        port = int(settings["port"])
        if not _is_port_available(port):
            if self._try_recover_port_from_stale_server(port):
                self.last_server_error = None
                self.last_server_line = None
            else:
                listeners = _get_listening_processes(port)
                owner_text = "another process"
                if listeners:
                    owner_parts = []
                    for info in listeners:
                        pid = str(info.get("Pid", "")).strip() or "?"
                        name = str(info.get("Name", "")).strip() or "unknown"
                        owner_parts.append(f"{name} (PID {pid})")
                    owner_text = ", ".join(owner_parts)

                port_message = (
                    f"Port {port} is already in use by {owner_text}. "
                    "If the app is already running in the system tray, restore that instance instead of opening a second one. "
                    "Otherwise, stop the process using this port or change the app Port setting."
                )
                if show_dialogs:
                    self._show_error("Port In Use", port_message)
                self._append_log(f"[APP] Start skipped: {port_message}")
                self._set_status_state("ERROR", f"Port {port} is already in use.")
                self._set_last_event(f"Start blocked: port {port} in use.")
                return False

        if not _is_port_available(port):
            port_message = (
                f"Port {port} is still not available after recovery attempt. "
                "Stop any process using this port or change the app Port setting."
            )
            if show_dialogs:
                self._show_error("Port In Use", port_message)
            self._append_log(f"[APP] Start skipped: {port_message}")
            self._set_status_state("ERROR", f"Port {port} is already in use.")
            self._set_last_event(f"Start blocked: port {port} in use.")
            return False

        configured_base_url = str(settings["base_url"]).strip()
        base_url = self._effective_base_url(configured_base_url, settings["port"])
        if not configured_base_url:
            self._append_log(f"[APP] Base URL not set. Using auto-detected address: {base_url}")
        elif self._is_local_only_base_url(configured_base_url) and base_url.rstrip("/") != configured_base_url.rstrip("/"):
            self._append_log(
                f"[APP] Base URL '{configured_base_url}' is local-only. Using '{base_url}' so other devices can reach streams."
            )
        env = os.environ.copy()
        for key in list(env):
            if (
                key.startswith("M3U_URL_")
                or key.startswith("M3U_MAX_CONCURRENCY_")
                or key.startswith("INCLUDE_TITLE_")
                or key.startswith("EXCLUDE_TITLE_")
                or key.startswith("CHANNEL_SOURCES_")
                or key.startswith("CHANNEL_MERGE_")
                or key.startswith("DISCOVERY_JOB_")
            ):
                env.pop(key, None)

        env["PORT"] = settings["port"]
        env["BASE_URL"] = base_url
        env["TZ"] = settings["timezone"]
        env["SYNC_CRON"] = settings["sync_cron"]
        env["SYNC_ON_BOOT"] = _to_bool_env(settings["sync_on_boot"])
        env["CLEAR_ON_BOOT"] = _to_bool_env(settings["clear_on_boot"])
        env["CREDENTIALS"] = settings["credentials"]
        env["MAX_RETRIES"] = settings["max_retries"]
        env["RETRY_WAIT"] = settings["retry_wait"]
        env["STREAM_TIMEOUT"] = settings["stream_timeout"]
        env["DATA_PATH"] = str(DATA_DIR)
        env["TEMP_PATH"] = str(TEMP_DIR)

        for index, source in enumerate(settings["sources"], start=1):
            source_url = str(source.get("url", "")).strip()
            if not source_url:
                continue

            concurrency = str(source.get("concurrency", "1")).strip() or "1"
            if not concurrency.isdigit() or int(concurrency) < 1:
                concurrency = "1"

            env[f"M3U_URL_{index}"] = source_url
            env[f"M3U_MAX_CONCURRENCY_{index}"] = concurrency

        for index, pattern in enumerate(settings["include_title_filters"], start=1):
            value = str(pattern).strip()
            if value:
                env[f"INCLUDE_TITLE_{index}"] = value

        channel_merge_rules: list[dict[str, str]] = []
        for rule in settings["channel_merge_rules"]:
            source = str(rule.get("source", "")).strip()
            target = str(rule.get("target", "")).strip()
            if not source or not target or source.casefold() == target.casefold():
                continue
            channel_merge_rules.append({"source": source, "target": target})

        # Guarantee merged source titles do not appear as separate playlist channels.
        # Parsing applies CHANNEL_MERGE before EXCLUDE_TITLE filters.
        effective_exclude_patterns: list[str] = []
        seen_exclude_patterns: set[str] = set()
        for pattern in settings["exclude_title_filters"]:
            value = str(pattern).strip()
            if not value:
                continue
            value_key = value.casefold()
            if value_key in seen_exclude_patterns:
                continue
            seen_exclude_patterns.add(value_key)
            effective_exclude_patterns.append(value)

        for rule in channel_merge_rules:
            source = rule["source"]
            value = self._channel_name_to_runtime_merge_exclude_pattern(source)
            if not value:
                continue
            value_key = value.casefold()
            if value_key in seen_exclude_patterns:
                continue
            seen_exclude_patterns.add(value_key)
            effective_exclude_patterns.append(value)

        for index, pattern in enumerate(effective_exclude_patterns, start=1):
            env[f"EXCLUDE_TITLE_{index}"] = pattern

        for index, rule in enumerate(settings["channel_source_rules"], start=1):
            pattern = str(rule.get("pattern", "")).strip()
            raw_sources = rule.get("sources", [])
            if not pattern or not isinstance(raw_sources, list):
                continue
            source_indexes = [str(s).strip() for s in raw_sources if str(s).strip()]
            if not source_indexes:
                continue
            env[f"CHANNEL_SOURCES_{index}"] = f"{pattern}|{','.join(source_indexes)}"

        for index, rule in enumerate(channel_merge_rules, start=1):
            env[f"CHANNEL_MERGE_{index}"] = f"{rule['source']}|{rule['target']}"

        for index, job in enumerate(settings.get("web_discovery_jobs", []), start=1):
            payload = self._normalize_web_discovery_jobs([job])
            if not payload:
                continue
            env[f"DISCOVERY_JOB_{index}"] = json.dumps(payload[0], separators=(",", ":"), sort_keys=True)

        discovery_job_count = len(self._normalize_web_discovery_jobs(settings.get("web_discovery_jobs", [])))

        if channel_merge_rules:
            self._append_log(
                "[APP] Applying "
                f"{len(channel_merge_rules)} channel merge rule(s) with "
                f"{len(effective_exclude_patterns)} effective exclude title filter(s)."
            )
        if discovery_job_count:
            self._append_log(f"[APP] Starting server with {discovery_job_count} web discovery job(s).")

        creation_flags = 0
        if os.name == "nt":
            creation_flags = subprocess.CREATE_NEW_PROCESS_GROUP
            if hasattr(subprocess, "CREATE_NO_WINDOW"):
                creation_flags |= subprocess.CREATE_NO_WINDOW

        try:
            self._set_status_state("STARTING", "Launching server process...")
            self._set_last_event("Starting server...")
            self.last_server_error = None
            self.last_server_line = None
            self.server_process = subprocess.Popen(
                [str(server_bin)],
                cwd=str(RUNTIME_DIR),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                stdin=subprocess.DEVNULL,
                env=env,
                text=True,
                encoding="utf-8",
                errors="replace",
                bufsize=1,
                creationflags=creation_flags,
            )
        except OSError as exc:
            self.server_process = None
            if show_dialogs:
                self._show_error("Start Failed", f"Could not start server:\n{exc}")
            else:
                self._append_log(f"[APP] Auto-start failed: {exc}")
            self._set_status_state("ERROR", "Server failed to launch.")
            self._set_last_event(f"Start failed: {exc}")
            return False

        self.last_exit_code = None
        threading.Thread(target=self._read_server_output, daemon=True).start()
        self._append_log(f"[APP] Server started on {base_url}.")
        self._set_last_event(f"Server started on {base_url}.")
        self._update_status_display()
        return True

    def _start_server_clicked(self) -> None:
        self._start_server(show_dialogs=True)

    def _autostart_server(self) -> None:
        if self.is_quitting:
            return
        self._append_log("[APP] Auto-starting server on app launch...")
        self._start_server(show_dialogs=False)

    def _restart_server_if_running(self, reason: str) -> None:
        proc = self.server_process
        if proc is None or proc.poll() is not None:
            return

        message = str(reason).strip() or "Settings updated."
        self._append_log(f"[APP] {message} Restarting server to apply changes...")
        self._set_last_event("Restarting server to apply changes...")
        self._stop_server_clicked()
        self._start_server(show_dialogs=False)

    def _stop_server_clicked(self) -> None:
        if not self.server_process:
            self.stop_requested_by_user = False
            self._set_last_event("Stop requested, but server was not running.")
            self._update_status_display()
            return

        self.stop_requested_by_user = True
        proc = self.server_process
        if proc.poll() is None:
            if os.name == "nt":
                try:
                    proc.send_signal(signal.CTRL_BREAK_EVENT)
                    proc.wait(timeout=2)
                except Exception:  # noqa: BLE001
                    pass

            if proc.poll() is None:
                try:
                    proc.terminate()
                    proc.wait(timeout=5)
                except Exception:  # noqa: BLE001
                    pass

            if proc.poll() is None:
                try:
                    proc.kill()
                except Exception:  # noqa: BLE001
                    pass

        self.last_exit_code = proc.poll()
        self._append_log("[APP] Server stopped.")
        self._set_last_event("Server stopped by user.")
        self._update_status_display()

    def _read_server_output(self) -> None:
        proc = self.server_process
        if proc is None or proc.stdout is None:
            return

        for line in proc.stdout:
            self.log_queue.put(line.rstrip())

        exit_code = proc.wait()
        self.log_queue.put(f"[server exited] code={exit_code}")

    def _drain_log_queue(self) -> None:
        try:
            while True:
                line = self.log_queue.get_nowait()
                self._append_log(line)
        except queue.Empty:
            pass
        finally:
            self.after(150, self._drain_log_queue)

    def _refresh_process_state(self) -> None:
        self._update_status_display()
        self.after(500, self._refresh_process_state)

    def _record_app_error(self, reason: str) -> None:
        clean_reason = str(reason).strip()
        if not clean_reason:
            return
        self._write_persistent_log(f"[APP-ERROR] {clean_reason}")
        self._write_error_report(clean_reason)

    def _show_error(self, title: str, message: str, parent: tk.Misc | None = None) -> None:
        self._record_app_error(f"{title}: {message}")
        if parent is None:
            messagebox.showerror(title, message)
        else:
            messagebox.showerror(title, message, parent=parent)

    def _read_detailed_error_log_text(self) -> str:
        sections: list[str] = []
        if APP_LOG_FILE.exists():
            try:
                sections.append("=== EVENT LOG ===")
                sections.append(APP_LOG_FILE.read_text(encoding="utf-8"))
            except OSError as exc:
                sections.append(f"Could not read {APP_LOG_FILE}: {exc}")
        else:
            sections.append("=== EVENT LOG ===")
            sections.append("No event log file found yet.")

        sections.append("")
        sections.append("=== DETAILED ERROR REPORTS ===")
        if APP_ERROR_LOG_FILE.exists():
            try:
                sections.append(APP_ERROR_LOG_FILE.read_text(encoding="utf-8"))
            except OSError as exc:
                sections.append(f"Could not read {APP_ERROR_LOG_FILE}: {exc}")
        else:
            sections.append("No detailed error reports found yet.")

        return "\n".join(sections)

    def _copy_error_log_selection(self) -> None:
        widget = self.error_log_text_widget
        if widget is None or not widget.winfo_exists():
            return
        try:
            selected = widget.get("sel.first", "sel.last")
        except tk.TclError:
            return
        self.clipboard_clear()
        self.clipboard_append(selected)

    def _refresh_error_log_view(self) -> None:
        widget = self.error_log_text_widget
        if widget is None or not widget.winfo_exists():
            return
        text = self._read_detailed_error_log_text()
        widget.configure(state=tk.NORMAL)
        widget.delete("1.0", tk.END)
        widget.insert(tk.END, text)
        widget.see(tk.END)
        widget.configure(state=tk.DISABLED)

    def _open_error_log_clicked(self) -> None:
        if self.error_log_window is not None and self.error_log_window.winfo_exists():
            self._refresh_error_log_view()
            self.error_log_window.deiconify()
            self.error_log_window.lift()
            self.error_log_window.focus_force()
            return

        popup = tk.Toplevel(self)
        popup.title("Detailed Error Log")
        popup.geometry("1100x720")
        popup.minsize(900, 560)
        popup.transient(self)

        root = ttk.Frame(popup, padding=10)
        root.pack(fill=tk.BOTH, expand=True)
        root.columnconfigure(0, weight=1)
        root.rowconfigure(0, weight=1)

        text_widget = tk.Text(root, wrap=tk.NONE, state=tk.DISABLED)
        text_widget.grid(row=0, column=0, sticky="nsew")
        scroll_y = ttk.Scrollbar(root, orient=tk.VERTICAL, command=text_widget.yview)
        scroll_y.grid(row=0, column=1, sticky="ns")
        scroll_x = ttk.Scrollbar(root, orient=tk.HORIZONTAL, command=text_widget.xview)
        scroll_x.grid(row=1, column=0, sticky="ew")
        text_widget.configure(yscrollcommand=scroll_y.set, xscrollcommand=scroll_x.set)

        actions = ttk.Frame(root)
        actions.grid(row=2, column=0, columnspan=2, sticky="e", pady=(8, 0))
        ttk.Button(actions, text="Refresh", command=self._refresh_error_log_view).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(actions, text="Copy Selected", command=self._copy_error_log_selection).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(actions, text="Open Log Folder", command=self._open_data_folder_clicked).pack(side=tk.LEFT, padx=(0, 8))
        ttk.Button(actions, text="Close", command=popup.destroy).pack(side=tk.LEFT)

        def copy_shortcut(_event: object) -> str:
            self._copy_error_log_selection()
            return "break"

        text_widget.bind("<Control-c>", copy_shortcut)
        text_widget.bind("<Button-3>", lambda _event: "break")

        def on_close_popup() -> None:
            self.error_log_window = None
            self.error_log_text_widget = None
            popup.destroy()

        popup.protocol("WM_DELETE_WINDOW", on_close_popup)

        self.error_log_window = popup
        self.error_log_text_widget = text_widget
        self._refresh_error_log_view()

    def _write_persistent_log(self, message: str) -> None:
        timestamp = time.strftime("%Y-%m-%d %H:%M:%S")
        line = f"{timestamp} {message}\n"
        try:
            APP_LOG_FILE.parent.mkdir(parents=True, exist_ok=True)
            with APP_LOG_FILE.open("a", encoding="utf-8") as handle:
                handle.write(line)
        except OSError:
            pass

    def _settings_snapshot_for_error_log(self) -> dict[str, object]:
        safe_sources: list[dict[str, str]] = []
        for source in self.sources_items:
            safe_sources.append(
                {
                    "name": str(source.get("name", "")).strip(),
                    "url": str(source.get("url", "")).strip(),
                    "concurrency": str(source.get("concurrency", "")).strip(),
                }
            )

        return {
            "port": self.port_var.get().strip(),
            "base_url": self.base_url_var.get().strip(),
            "timezone": self.timezone_var.get().strip(),
            "sync_cron": self.sync_cron_var.get().strip(),
            "sync_on_boot": bool(self.sync_on_boot_var.get()),
            "clear_on_boot": bool(self.clear_on_boot_var.get()),
            "start_on_boot": bool(self.start_on_boot_var.get()),
            "sources": safe_sources,
            "include_title_filters": list(self.include_title_filters),
            "exclude_title_filters": list(self.exclude_title_filters),
            "channel_source_rules": list(self.channel_source_rules),
            "channel_merge_rules": list(self.channel_merge_rules),
            "web_discovery_jobs": [dict(job) for job in self.web_discovery_jobs],
        }

    def _write_error_report(self, reason: str) -> None:
        timestamp = time.strftime("%Y-%m-%d %H:%M:%S")
        recent_lines = list(self.recent_log_lines)[-80:]
        payload = {
            "timestamp": timestamp,
            "reason": reason,
            "last_exit_code": self.last_exit_code,
            "last_server_error": self.last_server_error,
            "last_server_line": self.last_server_line,
            "stop_requested_by_user": self.stop_requested_by_user,
            "settings": self._settings_snapshot_for_error_log(),
            "recent_log_lines": recent_lines,
        }
        try:
            APP_ERROR_LOG_FILE.parent.mkdir(parents=True, exist_ok=True)
            with APP_ERROR_LOG_FILE.open("a", encoding="utf-8") as handle:
                handle.write(json.dumps(payload, indent=2))
                handle.write("\n")
                handle.write("-" * 80)
                handle.write("\n")
            self._write_persistent_log(f"[APP] Detailed error report written: {APP_ERROR_LOG_FILE}")
        except OSError:
            pass

    def _append_log(self, message: str) -> None:
        clean = _strip_ansi(message).strip()
        if not clean:
            return

        self.recent_log_lines.append(clean)
        self._write_persistent_log(clean)

        if clean.startswith("[APP] "):
            self._set_last_event(clean[6:])
        elif clean.startswith("[server exited]"):
            code_text = clean.split("code=", 1)[1].strip() if "code=" in clean else "unknown"
            try:
                self.last_exit_code = int(code_text)
            except ValueError:
                self.last_exit_code = None
            if self.stop_requested_by_user:
                self._set_last_event("Server stopped by user.")
            else:
                self._set_last_event(f"Server exited (code: {code_text}).")
                if self.last_exit_code not in (None, 0):
                    self._write_error_report(f"Server exited with code {self.last_exit_code}.")
        elif (
            "startup error:" in clean.lower()
            or
            "panic:" in clean.lower()
            or re.search(r"\bFTL\b", clean, re.IGNORECASE) is not None
            or "http server error:" in clean.lower()
            or "error initializing updater:" in clean.lower()
        ):
            self.last_server_line = clean
            self.last_server_error = clean
            if not self.stop_requested_by_user:
                self._set_status_state("ERROR", clean)
                self._set_last_event(clean)
                self._write_error_report(clean)
        else:
            self.last_server_line = clean

        self.log_text.configure(state=tk.NORMAL)
        self.log_text.insert(tk.END, clean + "\n")
        self.log_text.see(tk.END)
        self.log_text.configure(state=tk.DISABLED)

    def _on_close(self) -> None:
        if self.is_quitting:
            self._terminate_managed_server_processes()
            self._release_instance_lock()
            self.destroy()
            return

        self._append_log("[APP] Close requested; minimizing to tray...")
        self.withdraw()
        notify_text = "Application still running in tray."
        if self.server_process and self.server_process.poll() is None:
            notify_text = "Server still running."
            self._append_log("[APP] Window minimized to tray. Server still running.")
        else:
            self._append_log("[APP] Window minimized to tray.")

        if not self._show_tray(notify_text):
            self.deiconify()
            self.lift()
            self.focus_force()
            self._show_error(
                APP_NAME,
                "Could not create system tray icon. The app was restored to prevent it from exiting silently.",
            )
            self._append_log("[APP] Tray minimize failed; window restored.")


def main() -> None:
    app = DesktopApp()
    if app.startup_blocked:
        return
    app.mainloop()


if __name__ == "__main__":
    main()

