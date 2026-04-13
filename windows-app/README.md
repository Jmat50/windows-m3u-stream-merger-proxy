# Windows Desktop App

This folder turns `windows-m3u-stream-merger-proxy` into a Windows GUI application that bundles:

- the server binary (`windows-m3u-stream-merger-proxy.exe`)
- a desktop control panel (`gui_app.py` -> `WindowsM3UStreamMergerProxyDesktop.exe`)
- local app-managed data/temp directories

## What the GUI supports

- Start/stop the server
- Edit and save server settings
- Configure multiple M3U sources with concurrency values
- View live server logs
- Open playlist URL and app data folder quickly

## Build Requirements (Windows)

- Go (to compile `windows-m3u-stream-merger-proxy.exe`)
- Python 3.10+ (Tkinter included)
- Internet access for `pip install pyinstaller pystray pillow`

## Build Steps

From repository root:

```powershell
powershell -ExecutionPolicy Bypass -File .\windows-app\build.ps1
```

Output app folder:

```text
windows-app\dist\WindowsM3UStreamMergerProxyDesktop\
```

Run:

```text
windows-app\dist\WindowsM3UStreamMergerProxyDesktop\WindowsM3UStreamMergerProxyDesktop.exe
```

Behavior:

- The app auto-starts the bundled server on launch.
- Closing the window while the server is running minimizes the app to the system tray and shows `Server still running.`
- Right-click the tray icon and select `Exit` to stop the server and fully close the app.

## Runtime paths

Settings and runtime data are stored in:

```text
%LOCALAPPDATA%\WindowsM3UStreamMergerProxyDesktop
```

This keeps all server state self-contained in the desktop app profile.

