Name "SQL Exporter"
!define VERSION "98cffbac477fc532cb3bdab76709cd0dafce34f5"  ;git tag
!define INSTALLERVERSION "1.0.0.0"
!define DESCRIPTION "prometheus sql exporter for sql metrics"
OutFile "d:\sql_exporter_setup.exe"
!define _FWPORT "9237"

InstallDir $PROGRAMFILES64\sql_exporter

Page directory
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles


LoadLanguageFile "${NSISDIR}\Contrib\Language files\English.nlf"
;Version Information
VIProductVersion "${INSTALLERVERSION}"
VIAddVersionKey /LANG=${LANG_ENGLISH} "ProductName" "SQL Exporter"
VIAddVersionKey /LANG=${LANG_ENGLISH} "CompanyName" "<Your company>"
VIAddVersionKey /LANG=${LANG_ENGLISH} "LegalTrademarks" "Prometheus SQL exporter for SQL metrics"
VIAddVersionKey /LANG=${LANG_ENGLISH} "FileDescription" "sql_exporter_setup"
VIAddVersionKey /LANG=${LANG_ENGLISH} "FileVersion" "${VERSION}"


;Installation
Section ""
  ; install destination
  SetOutPath "$PROGRAMFILES64\sql_exporter"

  ; include all files from this directory
  File /R "d:\sql_exporter\*.*"
  
  ; create uninstaller
  WriteUninstaller $INSTDIR\uninstall.exe

  ; open the firewall port
  DetailPrint "Opening ${_FWPORT}/tcp for inbound connections.."
  ExecWait 'netsh advfirewall firewall add rule name="sql_exporter" protocol=TCP dir=in localport=${_FWPORT} action=allow'  

  ; service registering
  DetailPrint "Registering and starting service via nssm.."
  ExecWait '$PROGRAMFILES64\sql_exporter\supplementals\nssm\win64\nssm.exe install sql_exporter "$PROGRAMFILES64\sql_exporter\sql_exporter.exe"'
  ExecWait '$PROGRAMFILES64\sql_exporter\supplementals\nssm\win64\nssm.exe set sql_exporter Description "Prometheus sql_exporter for sql metrics"'
  ExecWait '$PROGRAMFILES64\sql_exporter\supplementals\nssm\win64\nssm.exe set sql_exporter DisplayName "SQL Exporter"'
  ExecWait '$PROGRAMFILES64\sql_exporter\supplementals\nssm\win64\nssm.exe start sql_exporter'

  ; add entry to Add/Remove Programs control panel
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\sql_exporter" "DisplayName" "sql_exporter"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\sql_exporter" "DisplayVersion" "${VERSION} x64 64Bit"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\sql_exporter" "Publisher" "credativ GmbH"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\sql_exporter" "UninstallString" "$PROGRAMFILES64\sql_exporter\uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\sql_exporter" "QuietUninstallString" "$PROGRAMFILES64\sql_exporter\uninstall.exe"

SectionEnd

; Uninstall
Section "Uninstall"
  ; service registering
  DetailPrint 'Removing service..'
  ExecWait '$PROGRAMFILES64\sql_exporter\supplementals\nssm\win64\nssm.exe stop sql_exporter'
  ExecWait '$PROGRAMFILES64\sql_exporter\supplementals\nssm\win64\nssm.exe remove sql_exporter confirm'

  ; close the firewall port
  DetailPrint "Closing ${_FWPORT}/tcp for inbound connections.."
  ExecWait 'netsh advfirewall firewall del rule name="sql_exporter" protocol=TCP dir=in localport=${_FWPORT}'  

  ; remove all files
  RMDir /r $INSTDIR
  
  ; delete the Registry Key 
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\sql_exporter"

SectionEnd
