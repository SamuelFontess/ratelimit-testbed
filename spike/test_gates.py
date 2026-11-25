import os
import shlex
import subprocess
from pathlib import Path

RAIZ = Path(__file__).resolve().parents[1]
G1 = RAIZ / "spike" / "g1_snat.sh"
GATE_LIB = RAIZ / "spike" / "gate_lib.sh"
TEMPO = 60


def bash(fonte, codigo, tmp_path, **ambiente):
    return subprocess.run(
        ["bash", "-c", f'source "{fonte}"\n{codigo}'],
        capture_output=True,
        text=True,
        timeout=TEMPO,
        env={**os.environ, "TMPDIR": str(tmp_path), **ambiente},
    )


def test_g1_rate_padrao_vem_da_constante(tmp_path):
    resultado = bash(
        G1,
        'printf "%s %s" "$RATE" "$DEFAULT_RATE"',
        tmp_path,
        CALIBRATION_ENV=str(tmp_path / "calibration.env"),
        G1_RATE="",
    )
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout == "2000 2000"


def test_g1_rate_usa_attack_rate_da_calibracao(tmp_path):
    calibracao = tmp_path / "calibration.env"
    calibracao.write_text("LAMBDA=5\nATTACK_RATE=1234\r\n")
    resultado = bash(
        G1,
        'printf "%s" "$RATE"',
        tmp_path,
        CALIBRATION_ENV=str(calibracao),
        G1_RATE="",
    )
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout == "1234"


def test_g1_rate_explicito_vence_a_calibracao(tmp_path):
    calibracao = tmp_path / "calibration.env"
    calibracao.write_text("ATTACK_RATE=1234\n")
    resultado = bash(
        G1,
        'printf "%s" "$RATE"',
        tmp_path,
        CALIBRATION_ENV=str(calibracao),
        G1_RATE="777",
    )
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout == "777"


def test_g1_calibracao_sem_attack_rate_cai_no_padrao(tmp_path):
    calibracao = tmp_path / "calibration.env"
    calibracao.write_text("LAMBDA=5\n")
    resultado = bash(
        G1,
        'printf "%s" "$RATE"',
        tmp_path,
        CALIBRATION_ENV=str(calibracao),
        G1_RATE="",
    )
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout == "2000"


def test_g1_min_fraction_continua_095(tmp_path):
    resultado = bash(G1, 'printf "%s" "$MIN_FRACTION"', tmp_path, G1_RATE="")
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout == "0.95"


def test_gate_falha_quando_o_teardown_falha(tmp_path):
    raiz = tmp_path / "repo"
    (raiz / "net").mkdir(parents=True)
    (raiz / "net" / "netns-teardown.sh").write_text("#!/bin/sh\nexit 3\n")
    resultado = bash(
        GATE_LIB,
        f"GATE_ROOT={shlex.quote(str(raiz))}\n"
        "GATE_NAME=teste\n"
        "SUDO=(env)\n"
        "trap gate_shutdown EXIT\n"
        "exit 0\n",
        tmp_path,
    )
    assert resultado.returncode == 1, resultado.stdout
    assert "FAIL teste: netns-teardown.sh saiu com 3" in resultado.stdout


def test_g1_ct_loss_tolera_ate_o_limite(tmp_path):
    codigo = """
ct_loss_ok "drop +1 insert_failed +0 early_drop +0" 10000 0.0001 && echo abaixo
ct_loss_ok "drop +0 insert_failed +1 early_drop +0" 10000 0.0001 && echo limite
ct_loss_ok "drop +1 insert_failed +1 early_drop +0" 10000 0.0001 || echo acima
ct_loss_ok "drop +0 insert_failed +0 early_drop +1" 1000000 0.0001 || echo early_drop
"""
    resultado = bash(G1, codigo, tmp_path, G1_RATE="")
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout.split() == ["abaixo", "limite", "acima", "early_drop"]


def test_g1_ct_loss_numbers_mostram_perda_e_fracoes(tmp_path):
    resultado = bash(
        G1,
        'ct_loss_numbers "drop +1 insert_failed +1 early_drop +0" 119978 0.0001',
        tmp_path,
        G1_RATE="",
    )
    assert resultado.returncode == 0, resultado.stderr
    assert resultado.stdout.strip() == (
        "drop +1 insert_failed +1 early_drop +0 "
        "(2 de 119978 conexões = 0,0017%, máximo 0,01%)"
    )


def test_g1_ct_loss_fraction_padrao_e_variavel(tmp_path):
    padrao = bash(
        G1,
        'printf "%s" "$CT_LOSS_MAX_FRACTION"',
        tmp_path,
        G1_RATE="",
        G1_CT_LOSS_MAX_FRACTION="",
    )
    trocado = bash(
        G1,
        'printf "%s" "$CT_LOSS_MAX_FRACTION"',
        tmp_path,
        G1_RATE="",
        G1_CT_LOSS_MAX_FRACTION="0.5",
    )
    assert padrao.stdout == "0.0001"
    assert trocado.stdout == "0.5"


def test_g1_ct_loss_fraction_invalida_e_rejeitada(tmp_path):
    resultado = bash(
        G1,
        'gate_fraction G1_CT_LOSS_MAX_FRACTION "$CT_LOSS_MAX_FRACTION"',
        tmp_path,
        G1_RATE="",
        G1_CT_LOSS_MAX_FRACTION="1.5",
    )
    assert resultado.returncode != 0
    assert "G1_CT_LOSS_MAX_FRACTION=1.5 inválido" in resultado.stdout
