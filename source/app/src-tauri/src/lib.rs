// f2f desktop app (Tauri v2): a standalone UI over the local f2f helper's
// loopback API (http://127.0.0.1:2202) plus helper-process management.
//
// - UI ↔ helper API: the frontend calls it via tauri-plugin-http (Rust-side
//   fetch, so the webview's cross-origin policy doesn't block localhost:2202).
// - helper process: detected/managed by the commands below. Bringing up the
//   tunnel needs root, so start/stop go through OS elevation (see helper_start).

use std::process::Command;

// helper_bin returns the absolute path of the `f2f` binary on PATH, or None if
// it isn't installed — the UI uses this to prompt an install when missing.
#[tauri::command]
fn helper_bin() -> Option<String> {
    let out = Command::new("sh")
        .arg("-c")
        .arg("command -v f2f")
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let p = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if p.is_empty() {
        None
    } else {
        Some(p)
    }
}

// helper_install downloads and installs the f2f binary via the official
// install.sh (to /usr/local/bin). It writes to a system dir, so it elevates.
// Runs detached — the status poll picks up the binary once it lands.
#[tauri::command]
fn helper_install() -> Result<(), String> {
    let script = "curl -fsSL https://raw.githubusercontent.com/vseplet/f2f/main/install.sh | sh";
    let spawn = |mut c: Command| c.spawn().map(|_| ()).map_err(|e| e.to_string());
    #[cfg(target_os = "macos")]
    {
        let mut c = Command::new("osascript");
        c.arg("-e").arg(format!(
            "do shell script \"{}\" with administrator privileges",
            script
        ));
        return spawn(c);
    }
    #[cfg(target_os = "linux")]
    {
        let mut c = Command::new("pkexec");
        c.arg("sh").arg("-c").arg(script);
        return spawn(c);
    }
    #[allow(unreachable_code)]
    Err("install is not supported on this OS yet".into())
}

// helper_start launches the interactive portal helper. The tunnel needs root,
// so we elevate per-OS (osascript on macOS, pkexec on Linux). Elevation UX is
// intentionally minimal here — refine per platform.
#[tauri::command]
fn helper_start() -> Result<(), String> {
    let spawn = |mut c: Command| c.spawn().map(|_| ()).map_err(|e| e.to_string());
    #[cfg(target_os = "macos")]
    {
        let mut c = Command::new("osascript");
        c.arg("-e")
            .arg("do shell script \"f2f\" with administrator privileges");
        return spawn(c);
    }
    #[cfg(target_os = "linux")]
    {
        let mut c = Command::new("pkexec");
        c.arg("f2f");
        return spawn(c);
    }
    #[allow(unreachable_code)]
    Err("helper start is not supported on this OS yet".into())
}

// helper_stop terminates a running helper (also privileged → elevates).
#[tauri::command]
fn helper_stop() -> Result<(), String> {
    let spawn = |mut c: Command| c.spawn().map(|_| ()).map_err(|e| e.to_string());
    #[cfg(target_os = "macos")]
    {
        let mut c = Command::new("osascript");
        c.arg("-e")
            .arg("do shell script \"pkill -f f2f\" with administrator privileges");
        return spawn(c);
    }
    #[cfg(target_os = "linux")]
    {
        let mut c = Command::new("pkexec");
        c.arg("pkill").arg("-f").arg("f2f");
        return spawn(c);
    }
    #[allow(unreachable_code)]
    Err("helper stop is not supported on this OS yet".into())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_http::init())
        .invoke_handler(tauri::generate_handler![
            helper_bin,
            helper_install,
            helper_start,
            helper_stop
        ])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
