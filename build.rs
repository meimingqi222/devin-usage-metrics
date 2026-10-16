fn main() {
    println!("cargo:rerun-if-changed=assets/icon.ico");
    println!("cargo:rerun-if-changed=assets/windows.manifest.xml");

    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        let resource_file =
            std::path::PathBuf::from(std::env::var_os("OUT_DIR").unwrap()).join("resource.rc");
        winres::WindowsResource::new()
            .set_icon(concat!(env!("CARGO_MANIFEST_DIR"), "/assets/icon.ico"))
            .set_manifest_file(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/assets/windows.manifest.xml"
            ))
            .write_resource_file(&resource_file)
            .expect("failed to generate the Windows application resources");

        // Native library linkage can omit resources from standalone integration tests.
        embed_resource::compile_for_everything(resource_file, embed_resource::NONE)
            .manifest_required()
            .expect("failed to embed the Windows application resources");
    }
}
