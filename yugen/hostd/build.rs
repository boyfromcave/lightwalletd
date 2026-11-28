// M-4: the commit this binary was built from, published by `yh_gettransparency`.
fn main() {
    let c = std::process::Command::new("git")
        .args(["describe", "--always", "--dirty", "--abbrev=12", "--match=NONE"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_else(|| "unknown".into());
    println!("cargo:rustc-env=YUGEN_HOSTD_COMMIT={c}");
    println!("cargo:rerun-if-changed=../../.git/HEAD");
    println!("cargo:rerun-if-changed=../../.git/index");
}
