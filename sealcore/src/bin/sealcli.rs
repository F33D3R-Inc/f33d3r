// sealcli — native CLI exercising the exact sealcore crypto the browser uses, so
// the sealed-mode server path can be verified end-to-end from a shell.
use sealcore::{generate_keypair_impl, open_message_impl, seal_message_impl};
use std::env;

fn main() {
    let a: Vec<String> = env::args().collect();
    let r = match a.get(1).map(String::as_str) {
        Some("keypair") => generate_keypair_impl(),
        Some("seal") if a.len() == 4 => seal_message_impl(&a[2], &a[3]),
        Some("open") if a.len() == 8 => {
            open_message_impl(&a[2], &a[3], &a[4], &a[5], &a[6], &a[7])
        }
        _ => Err("usage: sealcli <keypair | seal RECIPS TEXT | open PRIV EPH SEALED SNONCE BODYCT BODYNONCE>".into()),
    };
    match r {
        Ok(s) => println!("{s}"),
        Err(e) => {
            eprintln!("error: {e}");
            std::process::exit(1);
        }
    }
}
