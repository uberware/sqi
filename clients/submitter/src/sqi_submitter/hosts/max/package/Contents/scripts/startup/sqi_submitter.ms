-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- sqi submitter startup script for 3ds Max (loaded by the ApplicationPlugins package).
--
-- Defines the SubmitToSqi macroScript, which opens the shared submit dialog through
-- sqi_submitter (installed into 3ds Max's Python with pip), and registers a
-- "sqi > Submit to sqi..." main-menu entry.
--
-- 3ds Max 2024 gets the macroScript only: add it to a toolbar or quad via
-- Customize User Interface > Category "sqi". The menu registration below is wrapped in
-- try/catch so a Max build that lacks the callback or the menu calls still loads the
-- macroScript without error.

macroScript SubmitToSqi
    category:"sqi"
    internalCategory:"sqi"
    buttonText:"Submit to sqi..."
    toolTip:"Submit the current scene to sqi"
(
    on execute do
    (
        python.Execute "from sqi_submitter.hosts.max.menu import open_submitter; open_submitter()"
    )
)

-- Unique context id for this menu (first 32 bits of 4ec15823-3a42-4f66-9fc7-ccd0366768ee).
global sqiMenuContextId = 0x4ec15823

fn sqiRegisterMenus =
(
    try
    (
        if menuMan.registerMenuContext sqiMenuContextId then
        (
            local mainMenuBar = menuMan.getMainMenuBar()
            local sqiMenu = menuMan.findMenu "sqi"
            if sqiMenu == undefined then
            (
                sqiMenu = menuMan.createMenu "sqi"
                local actionItem = menuMan.createActionItem "SubmitToSqi" "sqi"
                sqiMenu.addItem actionItem -1
                local subMenuItem = menuMan.createSubMenuItem "sqi" sqiMenu
                mainMenuBar.addItem subMenuItem -1
            )
            menuMan.updateMenuBar()
        )
    )
    catch
    (
        format "sqi: could not register the main menu (%); use the SubmitToSqi macroScript instead.\n" (getCurrentException())
    )
)

try
(
    callbacks.removeScripts id:#sqiRegisterMenus
    callbacks.addScript #cuiRegisterMenus "sqiRegisterMenus()" id:#sqiRegisterMenus
)
catch
(
    format "sqi: menu callback unavailable (%); use the SubmitToSqi macroScript instead.\n" (getCurrentException())
)
